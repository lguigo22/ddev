package ddevapp_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ddev/ddev/pkg/ddevapp"
	"github.com/ddev/ddev/pkg/dockerutil"
	"github.com/ddev/ddev/pkg/fileutil"
	"github.com/ddev/ddev/pkg/globalconfig"
	"github.com/ddev/ddev/pkg/globalconfig/types"
	"github.com/ddev/ddev/pkg/testcommon"
	copy2 "github.com/otiai10/copy"
	asrt "github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.yaml.in/yaml/v3"
)

// TestTraefikSimple tests basic Traefik router usage
func TestTraefikSimple(t *testing.T) {
	if dockerutil.IsColima() || dockerutil.IsLima() || dockerutil.IsRancherDesktop() {
		// Intermittent failures in CI due apparently to https://github.com/lima-vm/lima/issues/2536
		// Expected port is not available, so it allocates another one.
		t.Skip("Skipping on Colima/Lima/Rancher because they don't predictably return ports")
	}

	assert := asrt.New(t)

	// Make sure this leaves us in the original test directory
	origDir, _ := os.Getwd()

	site := TestSites[0] // 0 == wordpress
	app, err := ddevapp.NewApp(site.Dir, true)
	assert.NoError(err)

	ddevapp.PowerOff()
	origRouter := globalconfig.DdevGlobalConfig.Router
	globalconfig.DdevGlobalConfig.Router = types.RouterTypeTraefik
	err = globalconfig.WriteGlobalConfig(globalconfig.DdevGlobalConfig)
	require.NoError(t, err)
	origConfig := *app

	t.Cleanup(func() {
		err = os.Chdir(origDir)
		assert.NoError(err)
		err = app.Stop(true, false)
		assert.NoError(err)
		ddevapp.PowerOff()
		err = origConfig.WriteConfig()
		assert.NoError(err)
		globalconfig.DdevGlobalConfig.Router = origRouter
		err = globalconfig.WriteGlobalConfig(globalconfig.DdevGlobalConfig)
		assert.NoError(err)
	})

	app.AdditionalHostnames = []string{"one", "two", "*.wild"}
	app.AdditionalFQDNs = []string{"onefullurl.ddev.site", "twofullurl.ddev.site", "*.wild.fqdn"}
	err = app.WriteConfig()
	require.NoError(t, err)
	err = app.StartAndWait(5)
	require.NoError(t, err)

	err = app.MutagenSyncFlush()
	require.NoError(t, err, "failed to flush Mutagen sync")

	desc, err := app.Describe(false)
	assert.Equal(desc["router"].(string), types.RouterTypeTraefik)

	// Test reachabiliity in each of the hostnames
	httpURLs, _, allURLs := app.GetAllURLs()

	// If no mkcert trusted https, use only the httpURLs
	// This is especially the case for Colima
	if globalconfig.GetCAROOT() == "" {
		allURLs = httpURLs
	}

	for _, u := range allURLs {
		// Use something here for wildcard
		u = strings.Replace(u, `*`, `somewildcard`, 1)
		_, err = testcommon.EnsureLocalHTTPContent(t, u+site.Safe200URIWithExpectation.URI, site.Safe200URIWithExpectation.Expect)
		assert.NoError(err, "failed EnsureLocalHTTPContent() %s: %v", u+site.Safe200URIWithExpectation.URI, err)
	}
}

// TestTraefikVirtualHost tests Traefik with an extra VIRTUAL_HOST
func TestTraefikVirtualHost(t *testing.T) {
	assert := asrt.New(t)

	// Make sure this leaves us in the original test directory
	origDir, _ := os.Getwd()

	site := TestSites[0] // 0 == wordpress
	app, err := ddevapp.NewApp(site.Dir, true)
	assert.NoError(err)

	ddevapp.PowerOff()
	origRouter := globalconfig.DdevGlobalConfig.Router
	globalconfig.DdevGlobalConfig.Router = "traefik"
	err = globalconfig.WriteGlobalConfig(globalconfig.DdevGlobalConfig)
	require.NoError(t, err)
	origConfig := *app

	t.Cleanup(func() {
		err = os.Chdir(origDir)
		assert.NoError(err)
		err = os.RemoveAll(app.GetConfigPath(`docker-compose.extra.yaml`))
		assert.NoError(err)
		err = app.Stop(true, false)
		assert.NoError(err)
		ddevapp.PowerOff()
		err = origConfig.WriteConfig()
		assert.NoError(err)
		globalconfig.DdevGlobalConfig.Router = origRouter
		err = globalconfig.WriteGlobalConfig(globalconfig.DdevGlobalConfig)
		assert.NoError(err)
	})

	err = fileutil.CopyFile(filepath.Join(origDir, "testdata", t.Name(), "docker-compose.extra.yaml"), app.GetConfigPath("docker-compose.extra.yaml"))
	require.NoError(t, err)

	err = app.StartAndWait(5)
	require.NoError(t, err)

	desc, err := app.Describe(false)
	assert.Equal(types.RouterTypeTraefik, desc["router"].(string))

	// Test reachabiliity in each of the hostnames
	httpURLs, _, allURLs := app.GetAllURLs()

	// If no mkcert trusted https, use only the httpURLs
	// This is especially the case for Colima
	if globalconfig.GetCAROOT() == "" {
		allURLs = httpURLs
	}

	for _, u := range allURLs {
		// Use something here for wildcard
		u = strings.Replace(u, `*`, `somewildcard`, 1)
		_, err = testcommon.EnsureLocalHTTPContent(t, u+site.Safe200URIWithExpectation.URI, site.Safe200URIWithExpectation.Expect)
		assert.NoError(err, "failed EnsureLocalHTTPContent() %s: %v", u+site.Safe200URIWithExpectation.URI, err)
	}

	// Test Reachability to nginx special VIRTUAL_HOST
	_, _ = testcommon.EnsureLocalHTTPContent(t, "http://extra.ddev.site", "Welcome to nginx")
	if globalconfig.DdevGlobalConfig.MkcertCARoot != "" {
		_, _ = testcommon.EnsureLocalHTTPContent(t, "https://extra.ddev.site", "Welcome to nginx")
	}
}

// TestTraefikStaticConfig tests static config usage and merging
func TestTraefikStaticConfig(t *testing.T) {
	if dockerutil.IsColima() || dockerutil.IsLima() || dockerutil.IsRancherDesktop() {
		// Intermittent failures in CI due apparently to https://github.com/lima-vm/lima/issues/2536
		// Expected port is not available, so it allocates another one.
		t.Skip("Skipping on Colima/Lima/Rancher because they don't predictably return ports")
	}
	origDir, _ := os.Getwd()
	globalTraefikDir := filepath.Join(globalconfig.GetGlobalDdevDir(), "traefik")
	staticConfigFinalPath := filepath.Join(globalTraefikDir, ".static_config.yaml")

	site := TestSites[0] // 0 == wordpress
	app, err := ddevapp.NewApp(site.Dir, true)
	require.NoError(t, err)

	testData := filepath.Join(origDir, "testdata", t.Name())

	err = app.Start()
	require.NoError(t, err)

	activeApps := ddevapp.GetActiveProjects()

	t.Cleanup(func() {
		_ = app.Stop(true, false)
		ddevapp.PowerOff()
	})

	testCases := []struct {
		content string
		dir     string
	}{
		{"logChange", "logChange"},
		{"extraPlugin", "extraPlugin"},
	}
	for _, tc := range testCases {
		t.Run("", func(t *testing.T) {
			testSourceDir := filepath.Join(testData, tc.dir)
			traefikGlobalConfigDir := filepath.Join(globalconfig.GetGlobalDdevDir(), "traefik")

			err = copy2.Copy(testSourceDir, traefikGlobalConfigDir)
			require.NoError(t, err)

			// Remove any static_config.*.yaml we have added
			t.Cleanup(func() {
				files, _ := filepath.Glob(filepath.Join(testSourceDir, "static_config.*.yaml"))
				for _, fileToRemove := range files {
					f := filepath.Base(fileToRemove)
					err = os.Remove(filepath.Join(traefikGlobalConfigDir, f))
					require.NoError(t, err)
				}
				err = os.Remove(filepath.Join(traefikGlobalConfigDir, "expectation.yaml"))
				require.NoError(t, err)
				err = ddevapp.PushGlobalTraefikConfig(activeApps)
				require.NoError(t, err)
			})

			// Unmarshal the loaded result expectation so it will look the same as merged (without comments, etc)
			var tmpMap map[string]interface{}
			expectedResultString, err := fileutil.ReadFileIntoString(filepath.Join(testSourceDir, "expectation.yaml"))
			require.NoError(t, err)
			err = yaml.Unmarshal([]byte(expectedResultString), &tmpMap)
			require.NoError(t, err)
			unmarshalledExpectationString, err := yaml.Marshal(tmpMap)
			require.NoError(t, err)

			// Generate and push config
			err = ddevapp.PushGlobalTraefikConfig(activeApps)
			require.NoError(t, err)
			// Now read result config and compare
			renderedStaticConfig, err := fileutil.ReadFileIntoString(staticConfigFinalPath)
			require.NoError(t, err)
			require.Equal(t, string(unmarshalledExpectationString), renderedStaticConfig)
		})
	}
}

// TestTraefikStaleFileCleanup tests that stale generated files are removed
// but user-managed files (without #ddev-generated) are preserved.
//
// The cleanup logic works by building an inventory of files from active projects'
// .ddev/traefik/ directories. Files in ~/.ddev/traefik/ that are NOT in this
// inventory AND have #ddev-generated are removed. Files without the signature
// are preserved as user-managed global config.
func TestTraefikStaleFileCleanup(t *testing.T) {
	globalTraefikDir := filepath.Join(globalconfig.GetGlobalDdevDir(), "traefik")
	globalConfigDir := filepath.Join(globalTraefikDir, "config")
	globalCertsDir := filepath.Join(globalTraefikDir, "certs")

	site := TestSites[0] // 0 == wordpress
	app, err := ddevapp.NewApp(site.Dir, true)
	require.NoError(t, err)

	ddevapp.PowerOff()

	// Files we'll create for testing
	staleFiles := []string{
		filepath.Join(globalConfigDir, "stale-project.yaml"),
		filepath.Join(globalConfigDir, "stale-project_middlewares.yaml"), // Additional config file
		filepath.Join(globalCertsDir, "stale-project.crt"),
		filepath.Join(globalCertsDir, "stale-project.key"),
		filepath.Join(globalConfigDir, "user-managed.yaml"),
		filepath.Join(globalConfigDir, "router_middlewares.yaml"), // Another user file
	}

	t.Cleanup(func() {
		_ = app.Stop(true, false)
		ddevapp.PowerOff()
		// Clean up test files
		for _, f := range staleFiles {
			_ = os.Remove(f)
		}
	})

	// Start the app to generate its config
	err = app.Start()
	require.NoError(t, err)

	// Get active projects
	activeApps := ddevapp.GetActiveProjects()
	require.NotEmpty(t, activeApps, "expected at least one active project")

	err = os.MkdirAll(globalConfigDir, 0755)
	require.NoError(t, err)
	err = os.MkdirAll(globalCertsDir, 0755)
	require.NoError(t, err)

	// Create stale generated files (with #ddev-generated signature) for a non-existent project.
	// These simulate files left over from a project that was previously running but is now stopped.
	staleConfigContent := "#ddev-generated\nhttp:\n  routers: {}\n"
	staleCertContent := "#ddev-generated\n-----BEGIN CERTIFICATE-----\nfake\n-----END CERTIFICATE-----\n"
	staleKeyContent := "#ddev-generated\n-----BEGIN PRIVATE KEY-----\nfake\n-----END PRIVATE KEY-----\n"

	// Main project config
	err = os.WriteFile(filepath.Join(globalConfigDir, "stale-project.yaml"), []byte(staleConfigContent), 0644)
	require.NoError(t, err)
	// Additional config file with different name (not just projectname.yaml)
	err = os.WriteFile(filepath.Join(globalConfigDir, "stale-project_middlewares.yaml"), []byte(staleConfigContent), 0644)
	require.NoError(t, err)
	// Certs
	err = os.WriteFile(filepath.Join(globalCertsDir, "stale-project.crt"), []byte(staleCertContent), 0644)
	require.NoError(t, err)
	err = os.WriteFile(filepath.Join(globalCertsDir, "stale-project.key"), []byte(staleKeyContent), 0644)
	require.NoError(t, err)

	// Create user-managed files (WITHOUT #ddev-generated signature).
	// These are global config files the user created directly in ~/.ddev/traefik/
	// and should be preserved regardless of what projects are running.
	userManagedContent := "# User customized config\nhttp:\n  middlewares:\n    my-middleware:\n      headers:\n        customRequestHeaders:\n          X-Custom-Header: \"value\"\n"
	err = os.WriteFile(filepath.Join(globalConfigDir, "user-managed.yaml"), []byte(userManagedContent), 0644)
	require.NoError(t, err)
	err = os.WriteFile(filepath.Join(globalConfigDir, "router_middlewares.yaml"), []byte(userManagedContent), 0644)
	require.NoError(t, err)

	// Verify all files exist before cleanup
	require.FileExists(t, filepath.Join(globalConfigDir, "stale-project.yaml"))
	require.FileExists(t, filepath.Join(globalConfigDir, "stale-project_middlewares.yaml"))
	require.FileExists(t, filepath.Join(globalCertsDir, "stale-project.crt"))
	require.FileExists(t, filepath.Join(globalCertsDir, "stale-project.key"))
	require.FileExists(t, filepath.Join(globalConfigDir, "user-managed.yaml"))
	require.FileExists(t, filepath.Join(globalConfigDir, "router_middlewares.yaml"))

	// Call PushGlobalTraefikConfig which should clean up stale files
	err = ddevapp.PushGlobalTraefikConfig(activeApps)
	require.NoError(t, err)

	// Verify stale generated files are removed (they have #ddev-generated
	// and are not in any active project's traefik directory)
	require.NoFileExists(t, filepath.Join(globalConfigDir, "stale-project.yaml"),
		"stale-project.yaml should be removed because it has #ddev-generated and is not in any active project")
	require.NoFileExists(t, filepath.Join(globalConfigDir, "stale-project_middlewares.yaml"),
		"stale-project_middlewares.yaml should be removed because it has #ddev-generated and is not in any active project")
	require.NoFileExists(t, filepath.Join(globalCertsDir, "stale-project.crt"),
		"stale-project.crt should be removed because it has #ddev-generated and is not in any active project")
	require.NoFileExists(t, filepath.Join(globalCertsDir, "stale-project.key"),
		"stale-project.key should be removed because it has #ddev-generated and is not in any active project")

	// Verify user-managed files are preserved (they don't have #ddev-generated)
	require.FileExists(t, filepath.Join(globalConfigDir, "user-managed.yaml"),
		"user-managed.yaml should be preserved because it does NOT have #ddev-generated")
	require.FileExists(t, filepath.Join(globalConfigDir, "router_middlewares.yaml"),
		"router_middlewares.yaml should be preserved because it does NOT have #ddev-generated")

	// Verify default files are preserved
	require.FileExists(t, filepath.Join(globalConfigDir, "default_config.yaml"),
		"default_config.yaml should always be preserved")

	// Verify active project files exist
	require.FileExists(t, filepath.Join(globalConfigDir, app.Name+".yaml"),
		"active project config should exist")
}
