// Package config defines the on-disk configuration for the sender.
//
// The config file is owned by the Electron main process: it writes the file
// at login (using values fetched from the auth API) and may patch it later
// when the user adjusts settings. The sender reads it at startup and may
// reload it on demand via the HTTP API.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/bharatpacs/tarang-sender/internal/dpapi"
)

// NotifierConfig sends study metadata to a backend URL the moment the first
// instance of a study arrives, so the study appears on the PACS immediately
// with an "upload_pending" status. Leave BackendURL empty to disable.
type NotifierConfig struct {
	BackendURL string `json:"backend_url"`
	APIKey     string `json:"api_key"`

	// EncryptedAPIKey is legacy per-field ciphertext, superseded by whole-file
	// encryption. Read on Load so an old config still works; blanked on Save.
	EncryptedAPIKey string `json:"encrypted_api_key,omitempty"`
}

// ErrorReportConfig controls automatic crash/error log reporting. When
// enabled, every error-level log record is spooled to disk and POSTed to
// BackendURL together with device and lab identification, so operators don't
// have to manually collect logs from the field. Leave BackendURL empty (or
// Enabled false) to disable.
type ErrorReportConfig struct {
	Enabled    bool   `json:"enabled"`
	BackendURL string `json:"backend_url"`
	APIKey     string `json:"api_key"`

	// EncryptedAPIKey is legacy per-field ciphertext — see NotifierConfig.
	EncryptedAPIKey string `json:"encrypted_api_key,omitempty"`
}

// LabStatusConfig controls the lab activation gate: before announcing a study
// and before uploading one, the sender asks the backend whether this lab is
// still allowed to transmit.
//
// The gate is deliberately asymmetric. Wrongly blocking a working lab loses
// studies (the retention sweeper deletes them within RetentionHours); wrongly
// allowing a lapsed one costs a few uploads. So "inactive" is only ever
// concluded from an answer the backend actually gave — an unreachable backend
// is "unknown", which keeps the last known verdict, and FailOpen decides only
// when there has never been a verdict at all.
//
// Set FailOpen false for a deny-by-default posture: nothing transmits until
// the backend confirms the lab is active.
type LabStatusConfig struct {
	Enabled     bool   `json:"enabled"`
	BackendURL  string `json:"backend_url"`
	APIKey      string `json:"api_key"`
	PollSeconds int    `json:"poll_seconds"`
	FailOpen    bool   `json:"fail_open"`
}

// ResilienceConfig controls the three dynamic resilience features.
// All default to true (enabled). Each can be toggled at runtime via
// PATCH /api/config without restarting the sender.
type ResilienceConfig struct {
	NetworkRetry         bool `json:"network_retry"`
	InstanceCheckpoint   bool `json:"instance_checkpoint"`
	ConnectivityWatchdog bool `json:"connectivity_watchdog"`
}

// Config is the root configuration object.
type Config struct {
	LabID         string              `json:"lab_id"`
	OrgID         string              `json:"org_id"`
	DICOM         DICOMConfig         `json:"dicom"`
	HTTP          HTTPConfig          `json:"http"`
	Storage       StorageConfig       `json:"storage"`
	Peer          PeerConfig          `json:"peer"`
	TagInjection  TagInjectionConfig  `json:"tag_injection"`
	Transfer      TransferConfig      `json:"transfer"`
	Compression   CompressionConfig   `json:"compression"`
	Notifier      NotifierConfig      `json:"notifier"`
	ErrorReport   ErrorReportConfig   `json:"error_reporting"`
	LabStatus     LabStatusConfig     `json:"lab_status"`
	Resilience    ResilienceConfig    `json:"resilience"`
	LogLevel      string              `json:"log_level"`

	// loadedFormat records how this config was stored on disk. Unexported so
	// it never round-trips into the file itself.
	loadedFormat Format
}

// DICOMConfig configures the C-STORE SCP listener.
//
// AET is display and logging only — the called AE title is never used to
// accept or reject an association. See internal/scp for why.
type DICOMConfig struct {
	Port             int             `json:"port"`
	AET              string          `json:"aet"`
	StableAgeSeconds int             `json:"stable_age_seconds"`
	TLS              DICOMTLSConfig  `json:"tls"`
}

// DICOMTLSConfig enables Secure DICOM on the SAME port as plaintext. Each
// connection is identified by its first byte, so enabling this costs nothing
// for modalities that send plaintext — which is why it defaults to on: a site
// that switches a modality to Secure DICOM then needs no sender change.
//
// Leave CertFile and KeyFile empty to use a generated, cached self-signed
// certificate. Supplying exactly one of them is a configuration error.
type DICOMTLSConfig struct {
	Enabled        bool   `json:"enabled"`
	CertFile       string `json:"cert_file"`
	KeyFile        string `json:"key_file"`
	AllowLegacyRC4 bool   `json:"allow_legacy_rc4"`
}

// HTTPConfig configures the local HTTP API used by the Electron renderer.
type HTTPConfig struct {
	Port     int    `json:"port"`
	APIToken string `json:"api_token"`

	// EncryptedAPIToken is legacy per-field ciphertext — see NotifierConfig.
	EncryptedAPIToken string `json:"encrypted_api_token,omitempty"`
}

// StorageConfig controls disk usage and retention.
type StorageConfig struct {
	DataDir         string `json:"data_dir"`
	RetentionHours  int    `json:"retention_hours"`
	MaxDiskGB       int    `json:"max_disk_gb"`
}

// PeerConfig describes the receiver-side Orthanc with the Transfers plugin.
type PeerConfig struct {
	Name        string `json:"name"`
	URL         string `json:"url"`
	Username    string `json:"username"`
	Password    string `json:"password"`
	Compression string `json:"compression"`
	CACertPath  string `json:"ca_cert_path"`

	// Protocol selects the push transport. Valid values: "stow-rs" (default,
	// requires the dicom-web plugin on the receiver) or "instances" (uses
	// Orthanc's native POST /instances and works against any Orthanc).
	Protocol string `json:"protocol,omitempty"`

	// Legacy per-field ciphertext — see NotifierConfig.
	EncryptedURL      string `json:"encrypted_url,omitempty"`
	EncryptedUsername string `json:"encrypted_username,omitempty"`
	EncryptedPassword string `json:"encrypted_password,omitempty"`
}

// TagInjectionConfig matches the semantics of the legacy tagwrite.lua.
//
// When enabled, on every received instance we write four private creator
// tags (group 0x0013/0x0015/0x0021/0x0043, element 0x0010) and four
// corresponding values at element 0x1060 in the same private blocks.
type TagInjectionConfig struct {
	Enabled              bool   `json:"enabled"`
	PrivateCreator       string `json:"private_creator"`
	PrivateOrganisation  string `json:"private_organisation"`
}

// TransferConfig tunes the Transfer Accelerator push client.
type TransferConfig struct {
	ConcurrentWorkers   int `json:"concurrent_workers"`
	BucketSizeMB        int `json:"bucket_size_mb"`
	MaxHTTPRetries      int `json:"max_http_retries"`
	HTTPTimeoutSeconds  int `json:"http_timeout_seconds"`
}

// CompressionConfig controls JPEG 2000 lossy transcoding via gdcmconv.
// When Enabled, series are transcoded before upload; ModalityRate maps
// modality codes (e.g. "CT", "MR") to an N:1 compression rate (rate 10 ≈
// 10:1). A smaller rate means higher quality / larger output; rate 0 disables
// compression for that modality.
type CompressionConfig struct {
	Enabled               bool           `json:"enabled"`
	SkipAlreadyCompressed bool           `json:"skip_already_compressed"`
	GdcmconvPath          string         `json:"gdcmconv_path"`
	DefaultRate           int            `json:"default_rate"`
	ModalityRate          map[string]int `json:"modality_rate"`

	// Lossless is a separate, independent mode — see LosslessConfig.
	Lossless LosslessConfig `json:"lossless"`
}

// LosslessConfig controls JPEG 2000 lossless (.90) transcoding.
//
// Both transcode modes ship OFF: studies are forwarded byte-for-byte as the
// modality sent them. Lossless is the safer of the two — its output is
// pixel-for-pixel identical to the input and typically 40-60% smaller — but
// it is still opt-in, so nothing rewrites pixel data unless a site turns it
// on deliberately. The lossy mode, which discards image data, is off as well.
//
// ModalityEnabled records EXCEPTIONS only, and is consulted only once Enabled
// is true. An absent modality is enabled, because lossless is safe everywhere
// and an empty map must not read as "disabled for everything".
// DefaultLosslessModalityPolicy seeds the exceptions we ship with.
type LosslessConfig struct {
	Enabled               bool            `json:"enabled"`
	SkipAlreadyCompressed bool            `json:"skip_already_compressed"`
	ModalityEnabled       map[string]bool `json:"modality_enabled"`
}

// DefaultLosslessModalityPolicy is the per-modality lossless policy we ship:
// CT, MR, CR and DX all on. It is inert while Compression.Lossless.Enabled is
// false (the shipped default) — it decides only WHICH modalities transcode
// once a site switches the mode on.
//
// History, because turning CR and DX on is a deliberate act and this is the
// record of why it was reversed:
//
// On 2026-08-29 every DX series transcoded to JPEG 2000 lossless came back from
// the receiving Orthanc as HTTP 400 with an empty ReferencedSOPSequence —
// nothing stored — while an untranscoded series from the same unit delivered
// normally. CR and DX were switched off as the conservative position.
//
// Two things changed underneath that finding afterwards. The bundled GDCM went
// from 3.2.6 (x64) to 3.2.7 (x86), so if the rejection was a 3.2.6 encoder bug
// it is no longer in play. And the same unit was separately sending one SOP
// Instance UID under two different Study UIDs, which Orthanc refuses outright
// and which nothing on this side can fix — so transcoding was never confirmed
// as the cause. CR and DX are back on to be retested against 3.2.7.
//
// To revert: set CR and/or DX back to false HERE, in
// LOSSLESS_MODALITY_POLICY in electron/main/main.js, and in
// examples/config.example.json. lossless_policy_test.go pins all three.
//
// Returns a fresh map on every call. Config.Load decodes the on-disk file ON
// TOP of these defaults, and encoding/json MERGES into an existing map rather
// than replacing it — a shared package-level map would be mutated by every
// config that names a modality.
func DefaultLosslessModalityPolicy() map[string]bool {
	return map[string]bool{
		"CT": true,
		"MR": true,
		"CR": true,
		"DX": true,
	}
}

// Default returns a Config populated with sensible defaults. Most fields are
// expected to be overridden via the on-disk JSON file; defaults are used only
// when fields are missing/zero, providing safe fallbacks.
func Default() Config {
	return Config{
		LogLevel: "info",
		DICOM: DICOMConfig{
			Port:             8899,
			AET:              "ACHYU",
			StableAgeSeconds: 5,
			TLS: DICOMTLSConfig{
				Enabled: true,
			},
		},
		HTTP: HTTPConfig{
			Port: 9044,
		},
		Storage: StorageConfig{
			RetentionHours: 24,
			MaxDiskGB:      50,
		},
		Peer: PeerConfig{
			Compression: "gzip",
		},
		TagInjection: TagInjectionConfig{
			Enabled: true,
		},
		Transfer: TransferConfig{
			ConcurrentWorkers:  6,
			BucketSizeMB:       0,
			MaxHTTPRetries:     3,
			HTTPTimeoutSeconds: 120,
		},
		Compression: CompressionConfig{
			// Both transcode modes ship OFF — see LosslessConfig.
			Enabled:               false,
			SkipAlreadyCompressed: true,
			GdcmconvPath:          "",
			DefaultRate:           0, // 0 = skip modalities not listed below
			ModalityRate: map[string]int{
				"CT": 10, // 10:1 ratio — inert while Enabled is false
			},
			Lossless: LosslessConfig{
				Enabled:               false,
				SkipAlreadyCompressed: true,
				ModalityEnabled:       DefaultLosslessModalityPolicy(),
			},
		},
		Notifier: NotifierConfig{
			BackendURL: "https://pacs.achyutrs.com/api/orthanc2/instance-exe-received",
		},
		ErrorReport: ErrorReportConfig{
			Enabled:    true,
			BackendURL: "https://pacs.achyutrs.com/api/sender/error-logs",
		},
		LabStatus: LabStatusConfig{
			Enabled:     true,
			BackendURL:  "https://pacs.achyutrs.com/api/sender/lab-status",
			PollSeconds: 60,
			FailOpen:    true,
		},
		Resilience: ResilienceConfig{
			NetworkRetry:         true,
			InstanceCheckpoint:   true,
			ConnectivityWatchdog: true,
		},
	}
}

// Load reads and validates a config file from disk.
//
// Missing fields are filled in from Default(). Invalid configurations return
// an error rather than silently coercing.
func Load(path string) (Config, error) {
	cfg := Default()

	raw, err := os.ReadFile(path)
	if err != nil {
		return cfg, fmt.Errorf("read config: %w", err)
	}

	// The file may be plaintext JSON, a Chromium v10 blob, or a raw DPAPI
	// blob — see crypto.go. A file that cannot be decrypted is an error, not
	// something to paper over with defaults: silently starting with a blank
	// config would mean transmitting under the wrong identity.
	b, format, err := decryptConfig(raw, path)
	if err != nil {
		return cfg, fmt.Errorf("read config (%s): %w", format, err)
	}
	cfg.loadedFormat = format

	// Decode on top of defaults so missing fields keep their default values.
	if err := json.Unmarshal(b, &cfg); err != nil {
		return cfg, fmt.Errorf("parse config: %w", err)
	}

	cfg.decryptLegacyFields()

	// Resolve a default DataDir relative to the config file if unset.
	if cfg.Storage.DataDir == "" {
		cfg.Storage.DataDir = filepath.Join(filepath.Dir(path), "data")
	}

	cfg.Normalize()

	if err := cfg.Validate(); err != nil {
		return cfg, err
	}

	return cfg, nil
}

// Normalize fills in derived defaults for fields that are present-but-zero in
// the on-disk file. Unlike Default(), which only applies to keys the file
// omits entirely, Normalize also repairs a key the file supplied as 0 — e.g.
// "poll_seconds": 0, which would otherwise spin the poll loop.
//
// It runs on Load and again on every PATCH /api/config, so a patch cannot
// route around these invariants.
func (c *Config) Normalize() {
	if c.LabStatus.PollSeconds <= 0 {
		c.LabStatus.PollSeconds = 60
	}
}

// Save writes the config to disk atomically, encrypted at rest.
//
// The value receiver is load-bearing: blanking the legacy ciphertext fields
// below mutates only this serialisation copy, so a caller holding the same
// Config keeps working with its plaintext secrets intact.
func (c Config) Save(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}

	// Legacy per-field ciphertext is superseded by whole-file encryption.
	// Writing it back would leave two copies of every secret, one of them
	// stale the moment somebody changes a password.
	c.HTTP.EncryptedAPIToken = ""
	c.Peer.EncryptedURL = ""
	c.Peer.EncryptedUsername = ""
	c.Peer.EncryptedPassword = ""
	c.Notifier.EncryptedAPIKey = ""
	c.ErrorReport.EncryptedAPIKey = ""

	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}

	out, _ := encryptConfig(b, path)

	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, out, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// SaveWithFormat is Save, additionally reporting which storage format was
// used. A caller that cares whether secrets ended up in cleartext — main.go
// does, at startup — uses this instead of Save.
func (c Config) SaveWithFormat(path string) (Format, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return FormatUnknown, err
	}

	c.HTTP.EncryptedAPIToken = ""
	c.Peer.EncryptedURL = ""
	c.Peer.EncryptedUsername = ""
	c.Peer.EncryptedPassword = ""
	c.Notifier.EncryptedAPIKey = ""
	c.ErrorReport.EncryptedAPIKey = ""

	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return FormatUnknown, err
	}

	out, format := encryptConfig(b, path)

	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, out, 0o600); err != nil {
		return format, err
	}
	return format, os.Rename(tmp, path)
}

// LoadedFormat reports the storage format this config was read from, so
// startup can log an upgrade from plaintext to ciphertext.
func (c Config) LoadedFormat() Format {
	if c.loadedFormat == "" {
		return FormatUnknown
	}
	return c.loadedFormat
}

// decryptLegacyFields fills plaintext secrets from any legacy per-field
// ciphertext left over from an older release. A field that already has a
// plaintext value wins — the whole-file format is the current source of truth
// and the legacy copy may be stale.
func (c *Config) decryptLegacyFields() {
	restore := func(plain *string, encrypted string) {
		if *plain != "" || encrypted == "" {
			return
		}
		if v, err := dpapi.Decrypt(encrypted); err == nil {
			*plain = v
		}
	}
	restore(&c.HTTP.APIToken, c.HTTP.EncryptedAPIToken)
	restore(&c.Peer.URL, c.Peer.EncryptedURL)
	restore(&c.Peer.Username, c.Peer.EncryptedUsername)
	restore(&c.Peer.Password, c.Peer.EncryptedPassword)
	restore(&c.Notifier.APIKey, c.Notifier.EncryptedAPIKey)
	restore(&c.ErrorReport.APIKey, c.ErrorReport.EncryptedAPIKey)
}

// Validate checks for obvious misconfigurations.
func (c Config) Validate() error {
	if c.LabID == "" {
		return errors.New("lab_id is required (set at login by Electron main)")
	}
	if c.OrgID == "" {
		return errors.New("org_id is required")
	}
	if c.DICOM.Port < 1 || c.DICOM.Port > 65535 {
		return fmt.Errorf("dicom.port out of range: %d", c.DICOM.Port)
	}
	if c.HTTP.Port < 1 || c.HTTP.Port > 65535 {
		return fmt.Errorf("http.port out of range: %d", c.HTTP.Port)
	}
	if c.DICOM.Port == c.HTTP.Port {
		return errors.New("dicom.port and http.port must differ")
	}
	if c.Storage.RetentionHours < 1 {
		return errors.New("storage.retention_hours must be at least 1")
	}
	if c.Peer.URL == "" {
		return errors.New("peer.url is required")
	}
	if c.Peer.Name == "" {
		return errors.New("peer.name is required")
	}
	switch c.Peer.Protocol {
	case "", "stow-rs", "instances":
		// valid
	default:
		return fmt.Errorf("peer.protocol must be \"stow-rs\" or \"instances\", got %q", c.Peer.Protocol)
	}
	if c.Transfer.ConcurrentWorkers < 1 {
		return errors.New("transfer.concurrent_workers must be at least 1")
	}
	// An enabled gate with no URL would silently never run, which is exactly
	// the state where studies transmit unchecked — fail loudly instead.
	if c.LabStatus.Enabled && c.LabStatus.BackendURL == "" {
		return errors.New("lab_status.backend_url is required when lab_status.enabled is true")
	}
	// Half a keypair means somebody edited one line and stopped. Both empty is
	// the supported "generate a self-signed cert" case; one of each is not.
	if (c.DICOM.TLS.CertFile == "") != (c.DICOM.TLS.KeyFile == "") {
		return errors.New("dicom.tls.cert_file and dicom.tls.key_file must be set together")
	}
	return nil
}
