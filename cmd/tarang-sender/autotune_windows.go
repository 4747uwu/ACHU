//go:build windows

package main

import (
	"syscall"
	"unsafe"
)

// memoryStatusEx mirrors MEMORYSTATUSEX from winbase.h.
type memoryStatusEx struct {
	dwLength                uint32
	dwMemoryLoad            uint32
	ullTotalPhys            uint64
	ullAvailPhys            uint64
	ullTotalPageFile        uint64
	ullAvailPageFile        uint64
	ullTotalVirtual         uint64
	ullAvailVirtual         uint64
	ullAvailExtendedVirtual uint64
}

var kernel32 = syscall.NewLazyDLL("kernel32.dll")
var procGlobalMemoryStatusEx = kernel32.NewProc("GlobalMemoryStatusEx")

// totalSystemRAMGB returns the physical RAM installed in the machine, in GB.
func totalSystemRAMGB() int {
	var ms memoryStatusEx
	ms.dwLength = uint32(unsafe.Sizeof(ms))
	//nolint:errcheck
	procGlobalMemoryStatusEx.Call(uintptr(unsafe.Pointer(&ms)))
	gb := int(ms.ullTotalPhys >> 30)
	if gb < 1 {
		return 4 // safe floor
	}
	return gb
}
