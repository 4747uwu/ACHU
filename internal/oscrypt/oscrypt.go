// Package oscrypt implements Chromium's "v10" OSCrypt format — what Electron's
// safeStorage actually emits on Windows.
//
// # The trap this package exists to close
//
// safeStorage output is NOT raw DPAPI. An earlier release assumed it was and
// had Go call CryptUnprotectData on the base64-decoded bytes, which produced
// "DPAPI decrypt failed: The data is invalid" in a crash loop on every
// workstation. A .NET ProtectedData round-trip appeared to prove interop, but
// .NET ProtectedData IS raw DPAPI — a false proxy for what Electron writes.
// The fix was implementing the real scheme, which is:
//
//	"v10" || 12-byte GCM nonce || AES-256-GCM ciphertext (16-byte tag appended)
//
// The key is not in the blob. It lives in Chromium's "Local State" JSON under
// os_crypt.encrypted_key: base64 → a 5-byte "DPAPI" prefix → a raw DPAPI blob
// wrapping the 32-byte AES key.
//
// crypto/cipher's NewGCM defaults to a 12-byte nonce and a 16-byte tag —
// exactly Chromium's parameters — so Go's Seal/Open are wire-compatible with
// safeStorage given the same key. Only LoadKey needs Windows; the AES-GCM half
// is pure stdlib and testable everywhere.
package oscrypt

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/bharatpacs/tarang-sender/internal/dpapi"
)

const (
	// V10Prefix marks a Chromium AES-256-GCM blob.
	V10Prefix = "v10"
	// KeyLen is the AES-256 key length in bytes.
	KeyLen = 32
	// nonceLen and tagLen are Chromium's GCM parameters, which are also Go's
	// defaults — stated here so a future change to either is visible.
	nonceLen = 12
	tagLen   = 16

	// dpapiKeyPrefix precedes the DPAPI-wrapped key inside Local State.
	dpapiKeyPrefix = "DPAPI"
)

// HasV10Prefix reports whether raw is a Chromium v10 blob.
func HasV10Prefix(raw []byte) bool {
	return len(raw) >= len(V10Prefix) && string(raw[:len(V10Prefix)]) == V10Prefix
}

// LoadKey extracts the AES-256 key from a Chromium "Local State" file.
//
// Requires DPAPI, so it only succeeds on Windows as the same user that wrote
// the file.
func LoadKey(localStatePath string) ([]byte, error) {
	b, err := os.ReadFile(localStatePath)
	if err != nil {
		return nil, fmt.Errorf("read local state: %w", err)
	}

	var state struct {
		OSCrypt struct {
			EncryptedKey string `json:"encrypted_key"`
		} `json:"os_crypt"`
	}
	if err := json.Unmarshal(b, &state); err != nil {
		return nil, fmt.Errorf("parse local state: %w", err)
	}
	if state.OSCrypt.EncryptedKey == "" {
		return nil, errors.New("local state has no os_crypt.encrypted_key")
	}

	wrapped, err := base64.StdEncoding.DecodeString(state.OSCrypt.EncryptedKey)
	if err != nil {
		return nil, fmt.Errorf("decode encrypted_key: %w", err)
	}
	if len(wrapped) <= len(dpapiKeyPrefix) || string(wrapped[:len(dpapiKeyPrefix)]) != dpapiKeyPrefix {
		return nil, errors.New("encrypted_key is not DPAPI-wrapped")
	}

	key, err := dpapi.DecryptBytes(wrapped[len(dpapiKeyPrefix):])
	if err != nil {
		return nil, fmt.Errorf("unwrap encrypted_key: %w", err)
	}
	if len(key) != KeyLen {
		return nil, fmt.Errorf("unwrapped key is %d bytes, want %d", len(key), KeyLen)
	}
	return key, nil
}

// EncryptV10 produces a blob that Electron's safeStorage.decryptString reads
// natively.
func EncryptV10(plain string, key []byte) ([]byte, error) {
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}

	nonce := make([]byte, nonceLen)
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("generate nonce: %w", err)
	}

	sealed := gcm.Seal(nil, nonce, []byte(plain), nil)

	out := make([]byte, 0, len(V10Prefix)+len(nonce)+len(sealed))
	out = append(out, V10Prefix...)
	out = append(out, nonce...)
	out = append(out, sealed...)
	return out, nil
}

// DecryptV10 reverses EncryptV10 and reads anything safeStorage wrote.
func DecryptV10(raw, key []byte) (string, error) {
	if !HasV10Prefix(raw) {
		return "", errors.New("oscrypt: not a v10 blob")
	}
	body := raw[len(V10Prefix):]
	if len(body) < nonceLen+tagLen {
		return "", errors.New("oscrypt: v10 blob is truncated")
	}

	gcm, err := newGCM(key)
	if err != nil {
		return "", err
	}

	nonce, ciphertext := body[:nonceLen], body[nonceLen:]
	plain, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		// Covers both a wrong key and tampering; GCM cannot tell them apart
		// and neither should the caller.
		return "", fmt.Errorf("oscrypt: decrypt failed: %w", err)
	}
	return string(plain), nil
}

func newGCM(key []byte) (cipher.AEAD, error) {
	if len(key) != KeyLen {
		return nil, fmt.Errorf("oscrypt: key is %d bytes, want %d", len(key), KeyLen)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}
