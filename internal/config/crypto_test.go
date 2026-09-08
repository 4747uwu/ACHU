package config

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/bharatpacs/tarang-sender/internal/dpapi"
)

// sampleConfig is a config with every kind of secret set, so a round-trip test
// can prove none of them survives in cleartext.
func sampleConfig() Config {
	c := Default()
	c.LabID = "ARX1"
	c.OrgID = "ARX"
	c.HTTP.APIToken = "0123456789abcdef0123456789abcdef"
	c.Peer.Name = "ACHYU"
	c.Peer.URL = "https://router.achyutrs.com"
	c.Peer.Username = "receiver-user"
	c.Peer.Password = "receiver-secret-password"
	c.Notifier.APIKey = "notifier-api-key"
	c.ErrorReport.APIKey = "error-api-key"
	c.Storage.DataDir = filepath.Join(os.TempDir(), "achyu-data")
	return c
}

func writeConfigFile(t *testing.T, dir string, body []byte) string {
	t.Helper()
	// Mirror the real layout: <userData>/achyu/config.json, with Local
	// State two directories up. localStatePath depends on this shape.
	appDir := filepath.Join(dir, "achyu")
	if err := os.MkdirAll(appDir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(appDir, "config.json")
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestDetectFormatLadder pins the one-byte discriminator. JSON always starts
// with '{' and base64 never can, which is the whole reason this works.
func TestDetectFormatLadder(t *testing.T) {
	v10 := base64.StdEncoding.EncodeToString(append([]byte("v10"), make([]byte, 40)...))
	dpapiish := base64.StdEncoding.EncodeToString([]byte{0x01, 0x00, 0x00, 0x00, 0xD0, 0x8C, 0x9D, 0xDF, 0x11, 0x22})

	cases := []struct {
		name string
		body string
		want Format
	}{
		{"plain json", `{"lab_id":"ARX1"}`, FormatPlaintext},
		{"json with leading whitespace", "  \n\t" + `{"lab_id":"ARX1"}`, FormatPlaintext},
		{"json with a UTF-8 BOM", "\uFEFF" + `{"lab_id":"ARX1"}`, FormatPlaintext},
		{"chromium v10 blob", v10, FormatAESGCMv10},
		{"raw dpapi blob", dpapiish, FormatDPAPI},
		{"empty file", "", FormatUnknown},
		{"garbage", "!!! not base64 !!!", FormatUnknown},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			if got := DetectFormat([]byte(tc.body)); got != tc.want {
				t.Fatalf("DetectFormat = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestPlaintextConfigStillLoadsAndIsUpgraded is the upgrade path every existing
// install takes on first run with this build.
func TestPlaintextConfigStillLoadsAndIsUpgraded(t *testing.T) {
	dir := t.TempDir()
	body, err := json.MarshalIndent(sampleConfig(), "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	path := writeConfigFile(t, dir, body)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load plaintext config: %v", err)
	}
	if cfg.LoadedFormat() != FormatPlaintext {
		t.Fatalf("LoadedFormat = %q, want plaintext", cfg.LoadedFormat())
	}
	if cfg.Peer.Password != "receiver-secret-password" {
		t.Fatalf("password did not survive load: %q", cfg.Peer.Password)
	}

	format, err := cfg.SaveWithFormat(path)
	if err != nil {
		t.Fatalf("save: %v", err)
	}

	// On Windows the re-save must have encrypted it. Elsewhere there is no OS
	// crypto, and plaintext is the documented fallback.
	if runtime.GOOS == "windows" {
		if format == FormatPlaintext {
			t.Fatal("config stayed plaintext on Windows — OS encryption should have applied")
		}
	}

	back, err := Load(path)
	if err != nil {
		t.Fatalf("reload after save: %v", err)
	}
	if back.Peer.Password != "receiver-secret-password" {
		t.Fatalf("password lost across save/load: %q", back.Peer.Password)
	}
}

// TestSaveEncryptsAndLoadRecovers is the property that matters: no secret is
// readable on disk, and every one of them comes back.
func TestSaveEncryptsAndLoadRecovers(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("OS encryption is Windows-only; plaintext fallback is covered elsewhere")
	}

	dir := t.TempDir()
	path := writeConfigFile(t, dir, []byte("{}"))

	original := sampleConfig()
	if _, err := original.SaveWithFormat(path); err != nil {
		t.Fatalf("save: %v", err)
	}

	onDisk, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{
		original.HTTP.APIToken,
		original.Peer.Password,
		original.Peer.Username,
		original.Notifier.APIKey,
		original.ErrorReport.APIKey,
	} {
		if strings.Contains(string(onDisk), secret) {
			t.Fatalf("secret %q is readable in the config file on disk", secret)
		}
	}

	back, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	for name, pair := range map[string][2]string{
		"api_token":     {original.HTTP.APIToken, back.HTTP.APIToken},
		"peer.url":      {original.Peer.URL, back.Peer.URL},
		"peer.username": {original.Peer.Username, back.Peer.Username},
		"peer.password": {original.Peer.Password, back.Peer.Password},
		"notifier.key":  {original.Notifier.APIKey, back.Notifier.APIKey},
		"errors.key":    {original.ErrorReport.APIKey, back.ErrorReport.APIKey},
		"lab_id":        {original.LabID, back.LabID},
	} {
		if pair[0] != pair[1] {
			t.Errorf("%s: got %q, want %q", name, pair[1], pair[0])
		}
	}
}

// TestSaveBlanksLegacyFields: writing the superseded per-field ciphertext back
// would leave two copies of every secret, the older one going stale the moment
// somebody changes a password.
func TestSaveBlanksLegacyFields(t *testing.T) {
	dir := t.TempDir()
	path := writeConfigFile(t, dir, []byte("{}"))

	cfg := sampleConfig()
	cfg.HTTP.EncryptedAPIToken = "AQAAANCMnd8-stale"
	cfg.Peer.EncryptedPassword = "AQAAANCMnd8-stale"

	if err := cfg.Save(path); err != nil {
		t.Fatalf("save: %v", err)
	}

	// Save takes a value receiver, so the caller's copy must be untouched.
	if cfg.HTTP.EncryptedAPIToken == "" {
		t.Fatal("Save mutated the caller's Config — the value receiver is load-bearing")
	}

	back, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if back.HTTP.EncryptedAPIToken != "" || back.Peer.EncryptedPassword != "" {
		t.Fatal("legacy ciphertext fields were written back to disk")
	}
	if back.HTTP.APIToken != cfg.HTTP.APIToken {
		t.Fatalf("plaintext token lost: %q", back.HTTP.APIToken)
	}
}

// TestLegacyPerFieldDecryption: an older config that stored secrets field by
// field must still open.
func TestLegacyPerFieldDecryption(t *testing.T) {
	if !dpapi.Available() {
		t.Skip("DPAPI unavailable")
	}

	enc, err := dpapi.Encrypt("legacy-password")
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}

	cfg := sampleConfig()
	cfg.Peer.Password = "" // only the ciphertext is present, as in the old format
	cfg.Peer.EncryptedPassword = enc

	body, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	path := writeConfigFile(t, t.TempDir(), body)

	back, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if back.Peer.Password != "legacy-password" {
		t.Fatalf("legacy password not recovered: %q", back.Peer.Password)
	}
}

// TestPlaintextWinsOverLegacyCiphertext: with the whole file already
// encrypted, the plaintext field is the current truth and a leftover legacy
// value must not overwrite it.
func TestPlaintextWinsOverLegacyCiphertext(t *testing.T) {
	if !dpapi.Available() {
		t.Skip("DPAPI unavailable")
	}
	stale, err := dpapi.Encrypt("stale-password")
	if err != nil {
		t.Fatal(err)
	}

	cfg := sampleConfig()
	cfg.Peer.Password = "current-password"
	cfg.Peer.EncryptedPassword = stale

	body, _ := json.Marshal(cfg)
	path := writeConfigFile(t, t.TempDir(), body)

	back, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if back.Peer.Password != "current-password" {
		t.Fatalf("stale legacy ciphertext overwrote the current password: %q", back.Peer.Password)
	}
}

// TestCorruptConfigFailsLoudly: starting with a blank config would mean
// transmitting under the wrong identity, so an undecryptable file must be an
// error rather than something papered over with defaults.
func TestCorruptConfigFailsLoudly(t *testing.T) {
	dir := t.TempDir()
	// Valid base64, not valid ciphertext.
	body := []byte(base64.StdEncoding.EncodeToString([]byte("this is not a real blob at all")))
	path := writeConfigFile(t, dir, body)

	if _, err := Load(path); err == nil {
		t.Fatal("a corrupt config file loaded without error")
	}
}

// TestBOMAndWhitespaceConfigLoads: a config opened and re-saved by a Windows
// text editor picks up a BOM, which must not be mistaken for ciphertext.
func TestBOMAndWhitespaceConfigLoads(t *testing.T) {
	body, err := json.Marshal(sampleConfig())
	if err != nil {
		t.Fatal(err)
	}
	path := writeConfigFile(t, t.TempDir(), append([]byte("\uFEFF\r\n  "), body...))

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("BOM-prefixed config failed to load: %v", err)
	}
	if cfg.LabID != "ARX1" {
		t.Fatalf("lab_id = %q", cfg.LabID)
	}
}

// TestNormalizeRepairsZeroPollSeconds: a present-but-zero value would spin the
// gate's poll loop, and Default() cannot help because the key is present.
func TestNormalizeRepairsZeroPollSeconds(t *testing.T) {
	cfg := sampleConfig()
	cfg.LabStatus.PollSeconds = 0
	cfg.Normalize()
	if cfg.LabStatus.PollSeconds != 60 {
		t.Fatalf("poll_seconds = %d, want 60", cfg.LabStatus.PollSeconds)
	}
}

// TestValidateRejectsHalfATLSKeypair: both empty is the supported
// "self-signed" case; one of each means somebody edited one line and stopped.
func TestValidateRejectsHalfATLSKeypair(t *testing.T) {
	cfg := sampleConfig()
	cfg.DICOM.TLS.CertFile = "C:/certs/dicom.pem"
	if err := cfg.Validate(); err == nil {
		t.Fatal("a half-configured TLS keypair validated")
	}
	cfg.DICOM.TLS.KeyFile = "C:/certs/dicom.key"
	if err := cfg.Validate(); err != nil {
		t.Fatalf("a complete TLS keypair was rejected: %v", err)
	}
}

// TestValidateRejectsEnabledGateWithNoURL: an enabled gate with no URL would
// silently never run, which is the state where studies transmit unchecked.
func TestValidateRejectsEnabledGateWithNoURL(t *testing.T) {
	cfg := sampleConfig()
	cfg.LabStatus.Enabled = true
	cfg.LabStatus.BackendURL = ""
	if err := cfg.Validate(); err == nil {
		t.Fatal("an enabled gate with no backend_url validated")
	}

	cfg.LabStatus.Enabled = false
	if err := cfg.Validate(); err != nil {
		t.Fatalf("a disabled gate with no URL was rejected: %v", err)
	}
}
