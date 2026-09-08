// Package dpapi wraps the Windows Data Protection API (CryptProtectData /
// CryptUnprotectData) for encrypting secrets at rest.
//
// Scope is CurrentUser with no additional entropy, which gives the property
// that matters here and one consequence that must be understood:
//
//	Property:    ciphertext can only be decrypted by the same Windows user on
//	             the same machine. Copying config.json off the workstation
//	             yields nothing useful.
//	Consequence: a config.json copied BETWEEN users or machines cannot be
//	             decrypted, and failing to do so is correct behaviour rather
//	             than a bug. Operators log in once after an upgrade that
//	             changes the user context.
//
// On non-Windows builds every function reports unavailable and passes data
// through unchanged, so the sender still runs for development on other
// platforms — the config layer falls back to plaintext there and says so.
package dpapi

import "encoding/base64"

// Encrypt protects plain and returns base64-encoded ciphertext.
func Encrypt(plain string) (string, error) {
	blob, err := EncryptBytes([]byte(plain))
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(blob), nil
}

// Decrypt reverses Encrypt.
func Decrypt(encryptedBase64 string) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(encryptedBase64)
	if err != nil {
		return "", err
	}
	out, err := DecryptBytes(raw)
	if err != nil {
		return "", err
	}
	return string(out), nil
}
