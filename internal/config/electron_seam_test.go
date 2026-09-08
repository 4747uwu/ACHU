package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// electronWrittenConfig is a byte-for-byte copy of the JSON that
// bootstrapSender in electron/main/main.js writes at login.
//
// Keep it in sync with that function by hand. That sounds fragile, and it is
// exactly the point: nothing in the Go compiler checks that the keys Electron
// writes match the struct tags here, so a renamed key fails silently — the
// field keeps its zero value and a feature quietly stops working in the field
// with no error anywhere. This test is the only thing standing at that seam.
const electronWrittenConfig = `{
  "lab_id": "ARX1",
  "org_id": "ARX",
  "log_level": "info",
  "dicom": {
    "port": 8888,
    "aet": "ACHYU",
    "stable_age_seconds": 15,
    "tls": {
      "enabled": true,
      "cert_file": "",
      "key_file": "",
      "allow_legacy_rc4": false
    }
  },
  "http": {
    "port": 9044,
    "api_token": "deadbeefdeadbeefdeadbeefdeadbeef"
  },
  "storage": {
    "data_dir": "C:\\Users\\Op\\AppData\\Roaming\\Achyu PACS\\achyu\\data",
    "retention_hours": 24,
    "max_disk_gb": 50
  },
  "peer": {
    "name": "ACHYU",
    "url": "https://router.achyutrs.com",
    "username": "Xcentic",
    "password": "XcenticIngestion123",
    "compression": "gzip",
    "ca_cert_path": "",
    "protocol": "stow-rs"
  },
  "tag_injection": {
    "enabled": true,
    "private_creator": "ARX1",
    "private_organisation": "ARX"
  },
  "transfer": {
    "concurrent_workers": 6,
    "bucket_size_mb": 32,
    "max_http_retries": 5,
    "http_timeout_seconds": 600
  },
  "compression": {
    "enabled": false,
    "skip_already_compressed": true,
    "gdcmconv_path": "C:\\Program Files\\Achyu PACS\\resources\\gdcmconv.exe",
    "default_rate": 0,
    "modality_rate": { "CT": 10 },
    "lossless": {
      "enabled": true,
      "skip_already_compressed": true,
      "modality_enabled": { "US": false }
    }
  },
  "notifier": {
    "backend_url": "https://pacs.achyutrs.com/api/orthanc2/instance-exe-received",
    "api_key": ""
  },
  "error_reporting": {
    "enabled": true,
    "backend_url": "https://pacs.achyutrs.com/api/sender/error-logs",
    "api_key": ""
  },
  "lab_status": {
    "enabled": true,
    "backend_url": "https://pacs.achyutrs.com/api/sender/lab-status",
    "api_key": "",
    "poll_seconds": 60,
    "fail_open": true
  },
  "resilience": {
    "network_retry": true,
    "instance_checkpoint": true,
    "connectivity_watchdog": true
  }
}`

// TestElectronWrittenConfigRoundTrips checks that every key Electron writes
// binds to a Go field, and that the resulting config is valid.
func TestElectronWrittenConfigRoundTrips(t *testing.T) {
	cfg := Default()
	if err := json.Unmarshal([]byte(electronWrittenConfig), &cfg); err != nil {
		t.Fatalf("Electron-written config does not parse: %v", err)
	}
	cfg.Normalize()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Electron-written config is invalid: %v", err)
	}

	checks := []struct {
		key  string
		got  any
		want any
	}{
		{"lab_id", cfg.LabID, "ARX1"},
		{"org_id", cfg.OrgID, "ARX"},
		// Deliberately NOT the shipped default (8899): a fixture that matches
		// the default would still pass if Load ignored the file entirely,
		// which is the exact failure this table exists to catch.
		{"dicom.port", cfg.DICOM.Port, 8888},
		{"dicom.aet", cfg.DICOM.AET, "ACHYU"},
		{"dicom.stable_age_seconds", cfg.DICOM.StableAgeSeconds, 15},
		{"dicom.tls.enabled", cfg.DICOM.TLS.Enabled, true},
		{"dicom.tls.allow_legacy_rc4", cfg.DICOM.TLS.AllowLegacyRC4, false},
		{"http.port", cfg.HTTP.Port, 9044},
		{"http.api_token", cfg.HTTP.APIToken, "deadbeefdeadbeefdeadbeefdeadbeef"},
		{"storage.retention_hours", cfg.Storage.RetentionHours, 24},
		{"storage.max_disk_gb", cfg.Storage.MaxDiskGB, 50},
		{"peer.name", cfg.Peer.Name, "ACHYU"},
		{"peer.url", cfg.Peer.URL, "https://router.achyutrs.com"},
		{"peer.username", cfg.Peer.Username, "Xcentic"},
		{"peer.password", cfg.Peer.Password, "XcenticIngestion123"},
		{"peer.protocol", cfg.Peer.Protocol, "stow-rs"},
		{"peer.compression", cfg.Peer.Compression, "gzip"},
		{"tag_injection.enabled", cfg.TagInjection.Enabled, true},
		{"tag_injection.private_creator", cfg.TagInjection.PrivateCreator, "ARX1"},
		{"tag_injection.private_organisation", cfg.TagInjection.PrivateOrganisation, "ARX"},
		{"transfer.concurrent_workers", cfg.Transfer.ConcurrentWorkers, 6},
		{"transfer.bucket_size_mb", cfg.Transfer.BucketSizeMB, 32},
		{"transfer.max_http_retries", cfg.Transfer.MaxHTTPRetries, 5},
		{"transfer.http_timeout_seconds", cfg.Transfer.HTTPTimeoutSeconds, 600},
		{"compression.enabled", cfg.Compression.Enabled, false},
		{"compression.default_rate", cfg.Compression.DefaultRate, 0},
		{"compression.lossless.enabled", cfg.Compression.Lossless.Enabled, true},
		{"compression.lossless.skip_already_compressed", cfg.Compression.Lossless.SkipAlreadyCompressed, true},
		{"notifier.backend_url", cfg.Notifier.BackendURL, "https://pacs.achyutrs.com/api/orthanc2/instance-exe-received"},
		{"error_reporting.enabled", cfg.ErrorReport.Enabled, true},
		{"error_reporting.backend_url", cfg.ErrorReport.BackendURL, "https://pacs.achyutrs.com/api/sender/error-logs"},
		{"lab_status.enabled", cfg.LabStatus.Enabled, true},
		{"lab_status.backend_url", cfg.LabStatus.BackendURL, "https://pacs.achyutrs.com/api/sender/lab-status"},
		{"lab_status.poll_seconds", cfg.LabStatus.PollSeconds, 60},
		{"lab_status.fail_open", cfg.LabStatus.FailOpen, true},
		{"resilience.network_retry", cfg.Resilience.NetworkRetry, true},
		{"resilience.instance_checkpoint", cfg.Resilience.InstanceCheckpoint, true},
		{"resilience.connectivity_watchdog", cfg.Resilience.ConnectivityWatchdog, true},
	}

	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v — the JSON key and the Go struct tag have drifted apart",
				c.key, c.got, c.want)
		}
	}

	if got := cfg.Compression.ModalityRate["CT"]; got != 10 {
		t.Errorf("compression.modality_rate[CT] = %d, want 10", got)
	}
	if v, ok := cfg.Compression.Lossless.ModalityEnabled["US"]; !ok || v {
		t.Errorf("compression.lossless.modality_enabled[US] = %v (present=%v), want false", v, ok)
	}
	if cfg.Storage.DataDir == "" {
		t.Error("storage.data_dir did not bind")
	}
}

// TestCompressionDefaultsOffButIsOperatorControlled: BOTH transcode modes must
// default off, an explicit opt-in must be honoured, and — the part that a
// previous "just clamp it in Normalize" approach broke — the opt-in has to
// survive a save/reload rather than being silently reset on next start.
//
// Lossless used to default ON. It is now off with the lossy mode: nothing
// rewrites pixel data unless a site asks for it, so a study leaves exactly as
// the modality sent it. Both halves are pinned here because "defaults off" is
// a shipping decision no other test would notice drifting.
func TestCompressionDefaultsOffButIsOperatorControlled(t *testing.T) {
	if got := Default().Compression.Enabled; got {
		t.Fatal("lossy compression defaults ON; it discards image data and must be opt-in")
	}
	if got := Default().Compression.Lossless.Enabled; got {
		t.Fatal("lossless compression defaults ON; both transcode modes ship off and must be opt-in")
	}

	dir := t.TempDir()
	path := writeConfigFile(t, dir, []byte("{}"))

	cfg := sampleConfig()
	cfg.Compression.Enabled = true
	cfg.Compression.DefaultRate = 12
	cfg.Compression.Lossless.Enabled = true

	if err := cfg.Save(path); err != nil {
		t.Fatalf("save: %v", err)
	}
	back, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if !back.Compression.Enabled {
		t.Error("an explicit lossy opt-in did not survive save/reload")
	}
	if back.Compression.DefaultRate != 12 {
		t.Errorf("default_rate = %d, want 12", back.Compression.DefaultRate)
	}
	// The opt-in direction is the one that can be silently lost now that the
	// default is false: a Normalize that clamped the mode off would erase it.
	if !back.Compression.Lossless.Enabled {
		t.Error("an explicit lossless opt-IN did not survive save/reload")
	}
}

// TestElectronBootstrapReadsConfigThroughDecrypter guards the seam's OTHER
// half: not "do the keys match" but "can Electron read back what it wrote".
//
// writeConfig() persists config.json as base64 AES-GCM via safeStorage, so
// the file's first byte is not '{' on any install that has ever been written.
// bootstrapSender used to open it with a bare
// JSON.parse(fs.readFileSync(CONFIG_PATH, 'utf8')) inside a try/catch that
// swallowed the error, so cfg fell back to {} on EVERY start and the function
// then rewrote the file from its own defaults.
//
// The symptom was remote from the cause and easy to misread as a save bug:
// an operator edits the DICOM port (or AE title, stable age, retention), the
// save works — the IPC handler decrypts correctly — and the value is back to
// default after the next restart. Nothing else in either half notices,
// because the token and ports are handed to the renderer fresh on each start
// and the peer credentials come from session.json.
//
// So this asserts the shape of the read, not its result: inside
// bootstrapSender the config must come from readConfig(), and the raw file
// must not be touched there at all.
func TestElectronBootstrapReadsConfigThroughDecrypter(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("..", "..", "electron", "main", "main.js"))
	if err != nil {
		t.Fatalf("read main.js: %v", err)
	}
	text := string(src)

	const start = "async function bootstrapSender("
	i := strings.Index(text, start)
	if i < 0 {
		t.Fatal("bootstrapSender not found in main.js — was it renamed?")
	}
	// The function ends at the writeConfig call that persists what it built.
	rest := text[i:]
	end := strings.Index(rest, "writeConfig(cfg);")
	if end < 0 {
		t.Fatal("could not find the writeConfig(cfg) that ends bootstrapSender")
	}
	body := rest[:end]

	if !strings.Contains(body, "readConfig()") {
		t.Error("bootstrapSender does not load the existing config through readConfig(); " +
			"every operator-edited setting will reset to default on restart")
	}
	if strings.Contains(body, "readFileSync(CONFIG_PATH") {
		t.Error("bootstrapSender reads config.json directly; the file is base64 AES-GCM, " +
			"so the parse throws and the config silently rebuilds from defaults")
	}
}
