package config

import (
	"os"
	"path/filepath"
	"testing"
)

// An existing install's config.json predates the api block. Upgrading the
// engine alone — the drop-in path — must still give it a fallback host, or
// failover would only reach sites that have logged in again since.
func TestExistingConfigGetsAPIDefaults(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")

	// A config as written before this feature existed: no "api" key at all.
	old := `{
  "lab_id": "LAB", "org_id": "ORG",
  "peer": {"name":"ACHYU","url":"https://router.achyutrs.com","protocol":"stow-rs"},
  "notifier": {"backend_url":"https://pacs.achyutrs.com/api/orthanc2/instance-exe-received"}
}`
	if err := os.WriteFile(path, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.API.BaseURL == "" {
		t.Fatal("api.base_url empty — an upgraded install would get no failover")
	}
	if len(cfg.API.FallbackBaseURLs) == 0 {
		t.Fatal("api.fallback_base_urls empty — nothing to fail over to")
	}
	t.Logf("base     : %s", cfg.API.BaseURL)
	t.Logf("fallbacks: %v", cfg.API.FallbackBaseURLs)
}
