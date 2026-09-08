//go:build !windows

package main

// totalSystemRAMGB returns a conservative default on non-Windows platforms.
func totalSystemRAMGB() int {
	return 8
}
