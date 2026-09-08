package oscrypt

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/bharatpacs/tarang-sender/internal/dpapi"
)

func testKey() []byte {
	k := make([]byte, KeyLen)
	for i := range k {
		k[i] = byte(i * 7)
	}
	return k
}

// TestEncryptDecryptRoundTrip covers the AES-GCM half, which is pure stdlib
// and so runs on every platform.
func TestEncryptDecryptRoundTrip(t *testing.T) {
	key := testKey()

	for _, plain := range []string{
		"",
		"x",
		`{"lab_id":"ARX1","peer":{"password":"receiver-secret"}}`,
		"unicode: naïve café — ✓",
	} {
		blob, err := EncryptV10(plain, key)
		if err != nil {
			t.Fatalf("encrypt %q: %v", plain, err)
		}
		if !HasV10Prefix(blob) {
			t.Fatalf("output for %q lacks the v10 prefix", plain)
		}
		// prefix + nonce + tag is the floor, even for empty plaintext.
		if len(blob) < len(V10Prefix)+nonceLen+tagLen {
			t.Fatalf("output for %q is too short: %d bytes", plain, len(blob))
		}

		back, err := DecryptV10(blob, key)
		if err != nil {
			t.Fatalf("decrypt %q: %v", plain, err)
		}
		if back != plain {
			t.Fatalf("round trip gave %q, want %q", back, plain)
		}
	}
}

// TestNonceIsFresh: reusing a nonce under the same key breaks GCM outright, so
// two encryptions of identical plaintext must differ.
func TestNonceIsFresh(t *testing.T) {
	key := testKey()
	a, err := EncryptV10("same plaintext", key)
	if err != nil {
		t.Fatal(err)
	}
	b, err := EncryptV10("same plaintext", key)
	if err != nil {
		t.Fatal(err)
	}
	if string(a) == string(b) {
		t.Fatal("two encryptions produced identical output — the nonce is being reused")
	}

	nonceA := a[len(V10Prefix) : len(V10Prefix)+nonceLen]
	nonceB := b[len(V10Prefix) : len(V10Prefix)+nonceLen]
	if string(nonceA) == string(nonceB) {
		t.Fatal("nonce repeated across encryptions")
	}
}

// TestDecryptRejectsTamperingAndWrongKey: GCM's authentication is the reason a
// modified config fails loudly instead of decrypting to garbage.
func TestDecryptRejectsTamperingAndWrongKey(t *testing.T) {
	key := testKey()
	blob, err := EncryptV10("sensitive", key)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("flipped ciphertext bit", func(t *testing.T) {
		bad := append([]byte(nil), blob...)
		bad[len(bad)-1] ^= 0x01
		if _, err := DecryptV10(bad, key); err == nil {
			t.Fatal("tampered ciphertext decrypted successfully")
		}
	})

	t.Run("flipped nonce bit", func(t *testing.T) {
		bad := append([]byte(nil), blob...)
		bad[len(V10Prefix)] ^= 0x01
		if _, err := DecryptV10(bad, key); err == nil {
			t.Fatal("tampered nonce decrypted successfully")
		}
	})

	t.Run("wrong key", func(t *testing.T) {
		other := testKey()
		other[0] ^= 0xFF
		if _, err := DecryptV10(blob, other); err == nil {
			t.Fatal("decrypted with the wrong key")
		}
	})

	t.Run("missing prefix", func(t *testing.T) {
		if _, err := DecryptV10(blob[len(V10Prefix):], key); err == nil {
			t.Fatal("a blob without the v10 prefix was accepted")
		}
	})

	t.Run("truncated blob", func(t *testing.T) {
		if _, err := DecryptV10(blob[:len(V10Prefix)+4], key); err == nil {
			t.Fatal("a truncated blob was accepted")
		}
	})

	t.Run("wrong key length", func(t *testing.T) {
		if _, err := EncryptV10("x", make([]byte, 16)); err == nil {
			t.Fatal("a 16-byte key was accepted for AES-256")
		}
	})
}

// TestLoadKeyAgainstSynthesizedLocalState builds a Local State file the way
// Chromium does — base64( "DPAPI" + DPAPI-wrapped 32-byte key ) — and checks
// we unwrap exactly that key back.
func TestLoadKeyAgainstSynthesizedLocalState(t *testing.T) {
	if !dpapi.Available() {
		t.Skip("DPAPI unavailable on this platform")
	}

	want := testKey()
	wrapped, err := dpapi.EncryptBytes(want)
	if err != nil {
		t.Fatalf("wrap key: %v", err)
	}

	state := map[string]any{
		"os_crypt": map[string]any{
			"encrypted_key": base64.StdEncoding.EncodeToString(append([]byte("DPAPI"), wrapped...)),
		},
	}
	body, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "Local State")
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := LoadKey(path)
	if err != nil {
		t.Fatalf("LoadKey: %v", err)
	}
	if string(got) != string(want) {
		t.Fatal("unwrapped key does not match the one that was wrapped")
	}

	// And the key actually works end to end.
	blob, err := EncryptV10("hello", got)
	if err != nil {
		t.Fatal(err)
	}
	if back, err := DecryptV10(blob, want); err != nil || back != "hello" {
		t.Fatalf("round trip with the loaded key failed: %q %v", back, err)
	}
}

// TestLoadKeyFailsClearly: every failure mode here ends with the config layer
// falling back to DPAPI, so they must be errors rather than a zero key.
func TestLoadKeyFailsClearly(t *testing.T) {
	dir := t.TempDir()

	cases := map[string]string{
		"missing file":    "",
		"not json":        "definitely not json",
		"no os_crypt":     `{"something":"else"}`,
		"empty key":       `{"os_crypt":{"encrypted_key":""}}`,
		"not base64":      `{"os_crypt":{"encrypted_key":"!!!!"}}`,
		"no DPAPI prefix": `{"os_crypt":{"encrypted_key":"` + base64.StdEncoding.EncodeToString([]byte("NOPE-plus-payload")) + `"}}`,
	}

	for name, body := range cases {
		name, body := name, body
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(dir, name)
			if body != "" {
				if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := LoadKey(path); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}

// TestHasV10Prefix guards the format discriminator used by the config layer.
func TestHasV10Prefix(t *testing.T) {
	if !HasV10Prefix([]byte("v10whatever")) {
		t.Error("v10 blob not recognised")
	}
	if HasV10Prefix([]byte("v1")) {
		t.Error("a too-short slice was treated as v10")
	}
	if HasV10Prefix(nil) {
		t.Error("nil was treated as v10")
	}
	// A raw DPAPI blob starts 01 00 00 00 D0 8C 9D DF.
	if HasV10Prefix([]byte{0x01, 0x00, 0x00, 0x00, 0xD0, 0x8C, 0x9D, 0xDF}) {
		t.Error("a raw DPAPI blob was misread as v10")
	}
}
