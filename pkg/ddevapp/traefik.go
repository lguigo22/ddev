package ddevapp

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/template"

	"github.com/ddev/ddev/pkg/dockerutil"
	"github.com/ddev/ddev/pkg/exec"
	"github.com/ddev/ddev/pkg/fileutil"
	"github.com/ddev/ddev/pkg/globalconfig"
	"github.com/ddev/ddev/pkg/nodeps"
	"github.com/ddev/ddev/pkg/util"
	copy2 "github.com/otiai10/copy"
)

type TraefikRouting struct {
	ExternalHostnames []string
	ExternalPort      string
	Service           struct {
		ServiceName         string
		InternalServiceName string
		InternalServicePort string
	}
	HTTPS bool
}

// detectAppRouting reviews the configured services and uses their
// VIRTUAL_HOST and HTTP(S)_EXPOSE environment variables to set up routing
// for the project
func detectAppRouting(app *DdevApp) ([]TraefikRouting, []string, error) {
	var table []TraefikRouting
	if app.ComposeYaml == nil || app.ComposeYaml.Services == nil {
		return table, nil, nil
	}
	for serviceName, service := range app.ComposeYaml.Services {
		var virtualHost string
		if virtualHostPointer, ok := service.Environment["VIRTUAL_HOST"]; ok && virtualHostPointer != nil && *virtualHostPointer != "" {
			virtualHost = *virtualHostPointer
			util.Debug("VIRTUAL_HOST=%v for %s", virtualHost, serviceName)
		}
		if virtualHost == "" {
			continue
		}
		hostnames := strings.Split(virtualHost, ",")
		if httpExposePointer, ok := service.Environment["HTTP_EXPOSE"]; ok && httpExposePointer != nil && *httpExposePointer != "" {
			httpExpose := *httpExposePointer
			util.Debug("HTTP_EXPOSE=%v for %s", httpExpose, serviceName)
			routeEntries, err := processHTTPExpose(serviceName, httpExpose, false, hostnames)
			if err != nil {
				return nil, nil, err
			}
			table = append(table, routeEntries...)
		}

		if httpsExposePointer, ok := service.Environment["HTTPS_EXPOSE"]; ok && httpsExposePointer != nil && *httpsExposePointer != "" {
			httpsExpose := *httpsExposePointer
			util.Debug("HTTPS_EXPOSE=%v for %s", httpsExpose, serviceName)
			routeEntries, err := processHTTPExpose(serviceName, httpsExpose, true, hostnames)
			if err != nil {
				return nil, nil, err
			}
			table = append(table, routeEntries...)
		}
	}

	hostnames := app.GetHostnames()
	// There can possibly be VIRTUAL_HOST entries which are not configured hostnames.
	for _, r := range table {
		if r.ExternalHostnames != nil {
			hostnames = append(hostnames, r.ExternalHostnames...)
		}
	}
	hostnames = util.SliceToUniqueSlice(&hostnames)

	return table, hostnames, nil
}

// processHTTPExpose creates routing table entry from VIRTUAL_HOST and HTTP(S)_EXPOSE
// environment variables
func processHTTPExpose(serviceName string, httpExpose string, isHTTPS bool, externalHostnames []string) ([]TraefikRouting, error) {
	var routingTable []TraefikRouting
	portPairs := strings.Split(httpExpose, ",")
	for _, portPair := range portPairs {
		ports := strings.Split(portPair, ":")
		if len(ports) == 0 || len(ports) > 2 {
			util.Warning("Skipping bad HTTP_EXPOSE port pair spec %s for service %s", portPair, serviceName)
			continue
		}
		if len(ports) == 1 {
			ports = append(ports, ports[0])
		}
		if ports[1] == "8025" && (globalconfig.DdevGlobalConfig.UseHardenedImages || globalconfig.DdevGlobalConfig.UseLetsEncrypt) {
			util.Debug("skipping port 8025 (mailpit) because not appropriate in hosting environment")
			continue
		}
		routingTable = append(routingTable, TraefikRouting{ExternalHostnames: externalHostnames, ExternalPort: ports[0],
			Service: struct {
				ServiceName         string
				InternalServiceName string
				InternalServicePort string
			}{
				ServiceName:         fmt.Sprintf("%s-%s", serviceName, ports[1]),
				InternalServiceName: serviceName,
				InternalServicePort: ports[1],
			}, HTTPS: isHTTPS})
	}
	return routingTable, nil
}

// PushGlobalTraefikConfig assembles traefik configuration from active projects and pushes
// it into the ddev-global-cache Docker volume.
//
// The process works as follows:
//  1. Build an inventory of all files that exist in active projects' .ddev/traefik/
//     directories (config/, certs/, and custom_certs/). This inventory is used to
//     identify which files in ~/.ddev/traefik/ are "expected" vs "stale".
//  2. Clean up stale files from ~/.ddev/traefik/config and ~/.ddev/traefik/certs:
//     - Files present in an active project's traefik directory are kept
//     - Files NOT in any active project but having #ddev-generated are removed (stale)
//     - Files NOT in any active project and lacking #ddev-generated are preserved
//     (these are user-created global config files)
//  3. Generate/update default global config (default_config.yaml, default certs,
//     .static_config.yaml with any static_config.*.yaml merges)
//  4. Copy config and certs from each active project into ~/.ddev/traefik/
//  5. Push the entire ~/.ddev/traefik/ directory to the Docker volume with
//     destroyExisting=true, ensuring the volume exactly matches the assembled config
//
// This ensures that only running projects have their routing active, while preserving
// user-customized global configuration files.
func PushGlobalTraefikConfig(activeApps []*DdevApp) error {
	globalTraefikDir := filepath.Join(globalconfig.GetGlobalDdevDir(), "traefik")
	uid, _, _ := dockerutil.GetContainerUser()
	err := os.MkdirAll(globalTraefikDir, 0755)
	if err != nil {
		return fmt.Errorf("failed to create global .ddev/traefik directory: %v", err)
	}
	globalSourceCertsPath := filepath.Join(globalTraefikDir, "certs")
	// SourceConfigDir for dynamic config
	globalSourceConfigDir := filepath.Join(globalTraefikDir, "config")
	inContainerTargetCertsPath := "/mnt/ddev-global-cache/traefik/certs"

	// Set up directories in ~/.ddev/traefik
	err = os.MkdirAll(globalSourceCertsPath, 0755)
	if err != nil {
		return fmt.Errorf("failed to create global Traefik certs dir: %v", err)
	}
	err = os.MkdirAll(globalSourceConfigDir, 0755)
	if err != nil {
		return fmt.Errorf("failed to create global Traefik config dir: %v", err)
	}

	// Build inventory of files from active projects' traefik directories.
	// This allows us to identify stale files that should be removed.
	activeConfigFiles := make(map[string]bool)
	activeCertFiles := make(map[string]bool)
	for _, app := range activeApps {
		// Enumerate config files from project's traefik/config directory
		projectConfigDir := app.GetConfigPath("traefik/config")
		if entries, err := os.ReadDir(projectConfigDir); err == nil {
			for _, entry := range entries {
				if !entry.IsDir() {
					activeConfigFiles[entry.Name()] = true
				}
			}
		}
		// Enumerate cert files from project's traefik/certs directory
		projectCertsDir := app.GetConfigPath("traefik/certs")
		if entries, err := os.ReadDir(projectCertsDir); err == nil {
			for _, entry := range entries {
				if !entry.IsDir() {
					activeCertFiles[entry.Name()] = true
				}
			}
		}
		// Also enumerate custom_certs directory (these get copied to global certs)
		projectCustomCertsDir := app.GetConfigPath("custom_certs")
		if entries, err := os.ReadDir(projectCustomCertsDir); err == nil {
			for _, entry := range entries {
				if !entry.IsDir() {
					activeCertFiles[entry.Name()] = true
				}
			}
		}
	}

	// Clean up stale generated files from config and certs directories.
	// Files not present in any active project AND having #ddev-generated are removed.
	// User-managed files (without #ddev-generated) are preserved.
	err = cleanStaleTraefikFiles(globalSourceConfigDir, activeConfigFiles, []string{"default_config.yaml"})
	if err != nil {
		util.Warning("Failed to clean stale Traefik config files: %v", err)
	}
	err = cleanStaleTraefikFiles(globalSourceCertsPath, activeCertFiles, []string{"default_cert.crt", "default_key.key"})
	if err != nil {
		util.Warning("Failed to clean stale Traefik cert files: %v", err)
	}

	// Assume that the #ddev-generated exists in file unless it doesn't
	sigExists := true
	for _, pemFile := range []string{"default_cert.crt", "default_key.key"} {
		origFile := filepath.Join(globalSourceCertsPath, pemFile)
		if fileutil.FileExists(origFile) {
			// Check to see if file has #ddev-generated in it, meaning we can recreate it.
			sigExists, err = fileutil.FgrepStringInFile(origFile, nodeps.DdevFileSignature)
			if err != nil {
				return err
			}
			// If either of the files has #ddev-generated, we will respect both
			if !sigExists {
				break
			}
		}
	}

	// If using Let's Encrypt, the default_cert.crt must not exist or
	// Traefik will use it.
	if globalconfig.DdevGlobalConfig.UseLetsEncrypt && sigExists {
		_ = os.RemoveAll(filepath.Join(globalSourceCertsPath, "default_cert.crt"))
		_ = os.RemoveAll(filepath.Join(globalSourceCertsPath, "default_key.key"))
	}
	// Install default certs, except when using Let's Encrypt (when they would
	// get used instead of Let's Encrypt certs)
	if !globalconfig.DdevGlobalConfig.UseLetsEncrypt && sigExists && globalconfig.DdevGlobalConfig.MkcertCARoot != "" {
		c := []string{"--cert-file", filepath.Join(globalSourceCertsPath, "default_cert.crt"), "--key-file", filepath.Join(globalSourceCertsPath, "default_key.key"), "127.0.0.1", "localhost", "*.ddev.local", "ddev-router", "ddev-router.ddev", "ddev-router.ddev_default", "*.ddev.site"}
		if globalconfig.DdevGlobalConfig.ProjectTldGlobal != "" {
			c = append(c, "*."+globalconfig.DdevGlobalConfig.ProjectTldGlobal)
		}

		out, err := exec.RunHostCommand("mkcert", c...)
		if err != nil {
			util.Failed("failed to create global mkcert certificate, check mkcert operation: %v", out)
		}

		// Prepend #ddev-generated in generated crt and key files
		for _, pemFile := range []string{"default_cert.crt", "default_key.key"} {
			origFile := filepath.Join(globalSourceCertsPath, pemFile)

			contents, err := fileutil.ReadFileIntoString(origFile)
			if err != nil {
				return fmt.Errorf("failed to read file %v: %v", origFile, err)
			}
			contents = nodeps.DdevFileSignature + "\n" + contents
			err = fileutil.TemplateStringToFile(contents, nil, origFile)
			if err != nil {
				return err
			}
		}
	}

	type traefikData struct {
		App                *DdevApp
		Hostnames          []string
		PrimaryHostname    string
		TargetCertsPath    string
		RouterPorts        []string
		UseLetsEncrypt     bool
		LetsEncryptEmail   string
		TraefikMonitorPort string
	}
	templateData := traefikData{
		TargetCertsPath:    inContainerTargetCertsPath,
		RouterPorts:        determineRouterPorts(activeApps),
		UseLetsEncrypt:     globalconfig.DdevGlobalConfig.UseLetsEncrypt,
		LetsEncryptEmail:   globalconfig.DdevGlobalConfig.LetsEncryptEmail,
		TraefikMonitorPort: globalconfig.DdevGlobalConfig.TraefikMonitorPort,
	}

	defaultConfigPath := filepath.Join(globalSourceConfigDir, "default_config.yaml")
	sigExists = true
	// TODO: Systematize this checking-for-signature, allow an arg to skip if empty
	fi, err := os.Stat(defaultConfigPath)
	// Don't use simple fileutil.FileExists() because of the danger of an empty file
	if err == nil && fi.Size() > 0 {
		// Check to see if file has #ddev-generated in it, meaning we can recreate it.
		sigExists, err = fileutil.FgrepStringInFile(defaultConfigPath, nodeps.DdevFileSignature)
		if err != nil {
			return err
		}
	}
	if !sigExists {
		util.Debug("Not creating %s because it exists and is managed by user", defaultConfigPath)
	} else {
		f, err := os.Create(defaultConfigPath)
		if err != nil {
			util.Failed("Failed to create Traefik config file: %v", err)
		}
		defer f.Close()
		t, err := template.New("traefik_global_config_template.yaml").Funcs(getTemplateFuncMap()).ParseFS(bundledAssets, "traefik_global_config_template.yaml")
		if err != nil {
			return fmt.Errorf("could not create template from traefik_global_config_template.yaml: %v", err)
		}

		err = t.Execute(f, templateData)
		if err != nil {
			return fmt.Errorf("could not parse traefik_global_config_template.yaml with templatedate='%v':: %v", templateData, err)
		}
	}

	staticConfigFinalPath := filepath.Join(globalTraefikDir, ".static_config.yaml")

	staticConfigTemp, err := os.CreateTemp("", "static_config-")
	if err != nil {
		return err
	}

	t, err := template.New("traefik_static_config_template.yaml").Funcs(getTemplateFuncMap()).ParseFS(bundledAssets, "traefik_static_config_template.yaml")
	if err != nil {
		return fmt.Errorf("could not create template from traefik_static_config_template.yaml: %v", err)
	}

	err = t.Execute(staticConfigTemp, templateData)
	if err != nil {
		return fmt.Errorf("could not parse traefik_static_config_template.yaml with templatedate='%v':: %v", templateData, err)
	}
	tmpFileName := staticConfigTemp.Name()
	err = staticConfigTemp.Close()
	if err != nil {
		return err
	}
	extraStaticConfigFiles, err := fileutil.GlobFilenames(globalTraefikDir, "static_config.*.yaml")
	if err != nil {
		return err
	}
	resultYaml, err := util.MergeYamlFiles(tmpFileName, extraStaticConfigFiles...)
	if err != nil {
		return err
	}
	err = os.WriteFile(staticConfigFinalPath, []byte(resultYaml), 0755)
	if err != nil {
		return err
	}

	// Copy active project configs, certs, and custom_certs into the global traefik directory,
	// so we can do a single CopyIntoVolume with destroyExisting=true.
	// This ensures only running projects have their routing active in the router.
	for _, app := range activeApps {
		projectConfigDir := app.GetConfigPath("traefik/config")
		projectCertsDir := app.GetConfigPath("traefik/certs")
		projectCustomCertsDir := app.GetConfigPath("custom_certs")

		// Copy project's config yaml to global config dir
		projectConfigFile := filepath.Join(projectConfigDir, app.Name+".yaml")
		if fileutil.FileExists(projectConfigFile) {
			destFile := filepath.Join(globalSourceConfigDir, app.Name+".yaml")
			err = fileutil.CopyFile(projectConfigFile, destFile)
			if err != nil {
				util.Warning("Failed to copy traefik config for project %s: %v", app.Name, err)
			}
		}

		// Copy project's certs to global certs dir
		for _, ext := range []string{".crt", ".key"} {
			projectCertFile := filepath.Join(projectCertsDir, app.Name+ext)
			if fileutil.FileExists(projectCertFile) {
				destFile := filepath.Join(globalSourceCertsPath, app.Name+ext)
				err = fileutil.CopyFile(projectCertFile, destFile)
				if err != nil {
					util.Warning("Failed to copy traefik cert for project %s: %v", app.Name, err)
				}
			}
		}

		// Copy project's custom_certs to global certs dir (if they exist)
		if fileutil.FileExists(filepath.Join(projectCustomCertsDir, app.Name+".crt")) {
			err = copy2.Copy(projectCustomCertsDir, globalSourceCertsPath)
			if err != nil {
				util.Warning("Failed to copy custom certs for project %s: %v", app.Name, err)
			} else {
				util.Debug("Copied custom certs from %s to global traefik certs dir", projectCustomCertsDir)
			}
		}
	}

	// Single copy with destroyExisting=true to clear stale configs from paused/stopped projects
	err = dockerutil.CopyIntoVolume(globalTraefikDir, "ddev-global-cache", "traefik", uid, "", true)
	if err != nil {
		return fmt.Errorf("failed to copy global Traefik config into Docker volume ddev-global-cache/traefik: %v", err)
	}
	util.Debug("Copied global Traefik config in %s to ddev-global-cache/traefik", globalTraefikDir)

	return nil
}

// configureTraefikForApp configures the dynamic configuration and creates cert+key
// in .ddev/traefik/certs
func configureTraefikForApp(app *DdevApp) error {
	routingTable, hostnames, err := detectAppRouting(app)
	if err != nil {
		return err
	}
	projectTraefikDir := app.GetConfigPath("traefik")
	err = os.MkdirAll(projectTraefikDir, 0755)
	if err != nil {
		return fmt.Errorf("failed to create .ddev/traefik directory: %v", err)
	}
	projectSourceCertsPath := filepath.Join(projectTraefikDir, "certs")
	projectSourceConfigDir := filepath.Join(projectTraefikDir, "config")
	inContainerTargetCertsPath := "/mnt/ddev-global-cache/traefik/certs"
	projectCustomCertsPath := app.GetConfigPath("custom_certs")

	err = os.MkdirAll(projectSourceCertsPath, 0755)
	if err != nil {
		return fmt.Errorf("failed to create project Traefik certs dir: %v", err)
	}
	err = os.MkdirAll(projectSourceConfigDir, 0755)
	if err != nil {
		return fmt.Errorf("failed to create project Traefik config dir: %v", err)
	}

	baseName := filepath.Join(projectSourceCertsPath, app.Name)
	// Assume that the #ddev-generated exists in file unless it doesn't
	sigExists := true
	for _, pemFile := range []string{app.Name + ".crt", app.Name + ".key"} {
		origFile := filepath.Join(projectSourceCertsPath, pemFile)
		if fileutil.FileExists(origFile) {
			// Check to see if file has #ddev-generated in it, meaning we can recreate it.
			sigExists, err = fileutil.FgrepStringInFile(origFile, nodeps.DdevFileSignature)
			if err != nil {
				return err
			}
			// If either of the files has #ddev-generated, we will respect both
			if !sigExists {
				break
			}
		}
	}
	// Assuming the certs don't exist, or they have #ddev-generated so can be replaced, create them
	// But not if we don't have mkcert already set up.
	if sigExists && globalconfig.DdevGlobalConfig.MkcertCARoot != "" {
		c := []string{"--cert-file", baseName + ".crt", "--key-file", baseName + ".key", "*.ddev.site", "127.0.0.1", "localhost", "*.ddev.local", "ddev-router", "ddev-router.ddev", "ddev-router.ddev_default"}
		c = append(c, hostnames...)
		if app.ProjectTLD != nodeps.DdevDefaultTLD {
			c = append(c, "*."+app.ProjectTLD)
		}
		out, err := exec.RunHostCommand("mkcert", c...)
		if err != nil {
			util.Failed("Failed to create certificates for project, check mkcert operation: %v; err=%v", out, err)
		}

		// Prepend #ddev-generated in generated crt and key files
		for _, pemFile := range []string{app.Name + ".crt", app.Name + ".key"} {
			origFile := filepath.Join(projectSourceCertsPath, pemFile)

			contents, err := fileutil.ReadFileIntoString(origFile)
			if err != nil {
				return fmt.Errorf("failed to read file %v: %v", origFile, err)
			}
			contents = nodeps.DdevFileSignature + "\n" + contents
			err = fileutil.TemplateStringToFile(contents, nil, origFile)
			if err != nil {
				return err
			}
		}
	}

	type traefikData struct {
		App             *DdevApp
		Hostnames       []string
		PrimaryHostname string
		TargetCertsPath string
		RoutingTable    []TraefikRouting
		UseLetsEncrypt  bool
	}
	templateData := traefikData{
		App:             app,
		Hostnames:       []string{},
		PrimaryHostname: app.GetHostname(),
		TargetCertsPath: inContainerTargetCertsPath,
		RoutingTable:    routingTable,
		UseLetsEncrypt:  globalconfig.DdevGlobalConfig.UseLetsEncrypt,
	}

	// Convert externalHostnames wildcards like `*.<anything>` to `[a-zA-Z0-9-]+.wild.ddev.site`
	for i, v := range routingTable {
		for j, h := range v.ExternalHostnames {
			if strings.HasPrefix(h, `*.`) {
				h = `[a-zA-Z0-9-]+` + strings.TrimPrefix(h, `*`)
				routingTable[i].ExternalHostnames[j] = h
			}
		}
	}

	traefikYamlFile := filepath.Join(projectSourceConfigDir, app.Name+".yaml")
	sigExists = true
	fi, err := os.Stat(traefikYamlFile)
	// Don't use simple fileutil.FileExists() because of the danger of an empty file
	if err == nil && fi.Size() > 0 {
		// Check to see if file has #ddev-generated in it, meaning we can recreate it.
		sigExists, err = fileutil.FgrepStringInFile(traefikYamlFile, nodeps.DdevFileSignature)
		if err != nil {
			return err
		}
	}
	if !sigExists {
		util.Debug("Not creating %s because it exists and is managed by user", traefikYamlFile)
	} else {
		f, err := os.Create(traefikYamlFile)
		if err != nil {
			return fmt.Errorf("failed to create Traefik config file: %v", err)
		}
		t, err := template.New("traefik_config_template.yaml").Funcs(getTemplateFuncMap()).ParseFS(bundledAssets, "traefik_config_template.yaml")
		if err != nil {
			return fmt.Errorf("could not create template from traefik_config_template.yaml: %v", err)
		}

		err = t.Execute(f, templateData)
		if err != nil {
			return fmt.Errorf("could not parse traefik_config_template.yaml with templatedate='%v':: %v", templateData, err)
		}
	}

	globalTraefikDir := filepath.Join(globalconfig.GetGlobalDdevDir(), "traefik")
	globalSourceCertsPath := filepath.Join(globalTraefikDir, "certs")
	if fileutil.FileExists(filepath.Join(projectCustomCertsPath, fmt.Sprintf("%s.crt", app.Name))) {
		err = copy2.Copy(projectCustomCertsPath, globalSourceCertsPath)
		if err != nil {
			util.Warning("Failed copying custom certs into global traefik certs dir: %v", err)
		} else {
			util.Debug("Copied custom certs in %s to global traefik certs dir", projectCustomCertsPath)
		}
	}

	uid, _, _ := dockerutil.GetContainerUser()
	err = dockerutil.CopyIntoVolume(projectTraefikDir, "ddev-global-cache", "traefik", uid, "", false)
	if err != nil {
		util.Warning("Failed to copy Traefik into Docker volume ddev-global-cache/traefik: %v", err)
	} else {
		util.Debug("Copied Traefik certs in %s to ddev-global-cache/traefik", projectSourceCertsPath)
	}

	return nil
}

// cleanStaleTraefikFiles removes stale files from a global traefik directory.
// It compares files in the global directory against files that exist in active projects'
// traefik directories. Files that are not present in any active project AND have
// the #ddev-generated signature are removed. Files in skipFiles are always preserved.
func cleanStaleTraefikFiles(dir string, activeProjectFiles map[string]bool, skipFiles []string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	// Build skipFiles map for fast lookup
	skipMap := make(map[string]bool)
	for _, f := range skipFiles {
		skipMap[f] = true
	}

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()

		// Skip files in the skip list (default files)
		if skipMap[name] {
			continue
		}

		// If this file exists in an active project's traefik directory, keep it
		if activeProjectFiles[name] {
			continue
		}

		filePath := filepath.Join(dir, name)

		// File is not from any active project. Check if it has #ddev-generated signature.
		// If so, it's a stale generated file and should be removed.
		// If not, it's a user-managed global config file and should be preserved.
		hasSignature, err := fileutil.FgrepStringInFile(filePath, nodeps.DdevFileSignature)
		if err != nil {
			// If we can't read the file, skip it
			util.Debug("Could not check signature of %s: %v", filePath, err)
			continue
		}

		if hasSignature {
			err = os.Remove(filePath)
			if err != nil {
				util.Warning("Failed to remove stale Traefik file %s: %v", filePath, err)
			} else {
				util.Debug("Removed stale Traefik file %s (not in any active project)", filePath)
			}
		} else {
			util.Debug("Preserving user-managed file %s (no #ddev-generated signature)", filePath)
		}
	}

	return nil
}
