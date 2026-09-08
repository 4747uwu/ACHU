//go:build !windows

package dpapi

import "errors"

// ErrUnsupported is returned on platforms without DPAPI.
var ErrUnsupported = errors.New("dpapi: only available on Windows")

// EncryptBytes is unsupported off Windows.
func EncryptBytes(plain []byte) ([]byte, error) { return nil, ErrUnsupported }

// DecryptBytes is unsupported off Windows.
func DecryptBytes(encrypted []byte) ([]byte, error) { return nil, ErrUnsupported }

// Available always reports false off Windows, which routes the config layer to
// its plaintext fallback so a development build still runs.
func Available() bool { return false }
