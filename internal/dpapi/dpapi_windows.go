//go:build windows

package dpapi

import (
	"bytes"
	"errors"
	"runtime"
	"syscall"
	"unsafe"
)

var (
	crypt32            = syscall.NewLazyDLL("crypt32.dll")
	kernel32           = syscall.NewLazyDLL("kernel32.dll")
	procCryptProtect   = crypt32.NewProc("CryptProtectData")
	procCryptUnprotect = crypt32.NewProc("CryptUnprotectData")
	procLocalFree      = kernel32.NewProc("LocalFree")
)

// cryptProtectUIForbidden stops Win32 from ever putting a dialog on screen.
// These workstations sit unattended at a reception desk; a blocked prompt
// nobody sees would look exactly like a hung sender.
const cryptProtectUIForbidden = 0x1

// dataBlob is the Win32 DATA_BLOB structure.
type dataBlob struct {
	cbData uint32
	pbData *byte
}

func newBlob(b []byte) dataBlob {
	if len(b) == 0 {
		return dataBlob{}
	}
	return dataBlob{cbData: uint32(len(b)), pbData: &b[0]}
}

// bytes copies the blob's contents into Go memory. The Win32 side allocated it
// with LocalAlloc, so the copy must happen before we LocalFree it.
func (b *dataBlob) bytes() []byte {
	if b.pbData == nil || b.cbData == 0 {
		return nil
	}
	out := make([]byte, b.cbData)
	copy(out, unsafe.Slice(b.pbData, b.cbData))
	return out
}

func (b *dataBlob) free() {
	if b.pbData != nil {
		_, _, _ = procLocalFree.Call(uintptr(unsafe.Pointer(b.pbData)))
		b.pbData = nil
	}
}

// EncryptBytes protects plain with DPAPI, returning the raw blob.
func EncryptBytes(plain []byte) ([]byte, error) {
	if len(plain) == 0 {
		return nil, errors.New("dpapi: nothing to encrypt")
	}
	in := newBlob(plain)
	var out dataBlob

	ret, _, err := procCryptProtect.Call(
		uintptr(unsafe.Pointer(&in)),
		0, // description
		0, // no additional entropy
		0, // reserved
		0, // no prompt struct — never show UI
		uintptr(cryptProtectUIForbidden),
		uintptr(unsafe.Pointer(&out)),
	)
	// Win32 holds a raw pointer into `plain` for the duration of the call, so
	// the GC must not be allowed to move or collect it before we return.
	runtime.KeepAlive(plain)

	if ret == 0 {
		return nil, wrapErr("CryptProtectData", err)
	}
	defer out.free()
	return out.bytes(), nil
}

// DecryptBytes reverses EncryptBytes.
func DecryptBytes(encrypted []byte) ([]byte, error) {
	if len(encrypted) == 0 {
		return nil, errors.New("dpapi: nothing to decrypt")
	}
	in := newBlob(encrypted)
	var out dataBlob

	ret, _, err := procCryptUnprotect.Call(
		uintptr(unsafe.Pointer(&in)),
		0, // description out
		0, // no entropy
		0, // reserved
		0, // no prompt struct
		uintptr(cryptProtectUIForbidden),
		uintptr(unsafe.Pointer(&out)),
	)
	runtime.KeepAlive(encrypted)

	if ret == 0 {
		return nil, wrapErr("CryptUnprotectData", err)
	}
	defer out.free()
	return out.bytes(), nil
}

// Available reports whether DPAPI actually works for this process, verified by
// a real round trip rather than by assuming it does because we are on Windows.
func Available() bool {
	probe := []byte("achyu-dpapi-probe")
	blob, err := EncryptBytes(probe)
	if err != nil {
		return false
	}
	out, err := DecryptBytes(blob)
	return err == nil && bytes.Equal(out, probe)
}

func wrapErr(call string, err error) error {
	if err == nil {
		return errors.New("dpapi: " + call + " failed")
	}
	return errors.New("dpapi: " + call + ": " + err.Error())
}
