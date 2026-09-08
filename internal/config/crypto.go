package config

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/bharatpacs/tarang-sender/internal/dpapi"
	"github.com/bharatpacs/tarang-sender/internal/oscrypt"
)

// The config file holds the API token and the receiver credentials, so it is
// encrypted at rest. Both halves of the product read and write it — Electron
// at login, Go at startup and on PATCH — so the two must agree on the format
// byte for byte.
//
// Three formats are accepted on read, and the detection ladder is one byte
// deep because JSON always starts with '{' and base64 never can:
//
//	first non-space byte == '{'  → plaintext JSON
//	else base64-decode, then:
//	    starts with "v10"        → AES-256-GCM with the Chromium Local State key
//	    else                     → raw DPAPI
//
// On write we prefer v10, because Electron's safeStorage reads it natively;
// then raw DPAPI, which safeStorage also reads via its own fallback; then
// plaintext, so a non-Windows development build still persists rather than
// failing outright.

// Format identifies how a config file is stored.
type Format string

const (
	FormatPlaintext Format = "plaintext-json"
	FormatAESGCMv10 Format = "aes-gcm-v10"
	FormatDPAPI     Format = "dpapi"
	FormatUnknown   Format = "unknown"
)

// DetectFormat identifies the storage format of raw config bytes.
func DetectFormat(raw []byte) Format {
	trimmed := trimStart(raw)
	if len(trimmed) == 0 {
		return FormatUnknown
	}
	if trimmed[0] == '{' {
		return FormatPlaintext
	}
	decoded, err := base64.StdEncoding.DecodeString(string(bytes.TrimSpace(trimmed)))
	if err != nil {
		return FormatUnknown
	}
	if oscrypt.HasV10Prefix(decoded) {
		return FormatAESGCMv10
	}
	return FormatDPAPI
}

// DescribeFile reports the format of the config file at path. It never returns
// an error — a file that cannot be read is simply unknown — so it is safe to
// call from a log line.
func DescribeFile(path string) Format {
	raw, err := os.ReadFile(path)
	if err != nil {
		return FormatUnknown
	}
	return DetectFormat(raw)
}

// trimStart strips leading whitespace and a UTF-8 BOM, so a config that has
// been opened and re-saved by a Windows text editor is still recognised as
// plaintext JSON rather than mistaken for ciphertext.
//
// The BOM is written as an escape on purpose: a literal BOM in the middle of a
// Go source file is a compile error.
func trimStart(b []byte) []byte {
	return bytes.TrimLeft(b, " \t\r\n\uFEFF")
}

// decryptConfig turns stored bytes into plaintext JSON.
func decryptConfig(raw []byte, configPath string) ([]byte, Format, error) {
	format := DetectFormat(raw)
	switch format {
	case FormatPlaintext:
		return trimStart(raw), format, nil

	case FormatAESGCMv10:
		decoded, err := base64.StdEncoding.DecodeString(string(bytes.TrimSpace(trimStart(raw))))
		if err != nil {
			return nil, format, fmt.Errorf("decode config: %w", err)
		}
		key, err := oscrypt.LoadKey(localStatePath(configPath))
		if err != nil {
			return nil, format, fmt.Errorf("load os_crypt key: %w", err)
		}
		plain, err := oscrypt.DecryptV10(decoded, key)
		if err != nil {
			return nil, format, err
		}
		return []byte(plain), format, nil

	case FormatDPAPI:
		decoded, err := base64.StdEncoding.DecodeString(string(bytes.TrimSpace(trimStart(raw))))
		if err != nil {
			return nil, format, fmt.Errorf("decode config: %w", err)
		}
		plain, err := dpapi.DecryptBytes(decoded)
		if err != nil {
			return nil, format, fmt.Errorf("dpapi decrypt config: %w", err)
		}
		return plain, format, nil
	}

	return nil, FormatUnknown, errors.New("config file is neither JSON nor recognised ciphertext")
}

// encryptConfig serialises plaintext JSON to the best available format,
// reporting which one it used.
//
// Preference order is v10 first because Electron reads it natively; raw DPAPI
// second because safeStorage falls back to it; plaintext last so a
// non-Windows dev build still works. A plaintext result on Windows means both
// crypto paths failed and the caller should say so loudly.
func encryptConfig(plain []byte, configPath string) ([]byte, Format) {
	if key, err := oscrypt.LoadKey(localStatePath(configPath)); err == nil {
		if blob, err := oscrypt.EncryptV10(string(plain), key); err == nil {
			return []byte(base64.StdEncoding.EncodeToString(blob)), FormatAESGCMv10
		}
	}
	if blob, err := dpapi.EncryptBytes(plain); err == nil {
		return []byte(base64.StdEncoding.EncodeToString(blob)), FormatDPAPI
	}
	return plain, FormatPlaintext
}

// localStatePath locates Chromium's Local State relative to the config file:
//
//	<userData>/achyu/config.json  →  <userData>/Local State
//
// If the sender is launched with a --config outside the Electron userData
// layout, this simply won't find a key and Save falls back to raw DPAPI, which
// both sides can still read.
func localStatePath(configPath string) string {
	return filepath.Join(filepath.Dir(filepath.Dir(configPath)), "Local State")
}
