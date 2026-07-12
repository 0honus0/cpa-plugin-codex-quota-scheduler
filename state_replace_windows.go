//go:build windows

package main

import (
	"syscall"
	"unsafe"
)

var moveFileEx = syscall.NewLazyDLL("kernel32.dll").NewProc("MoveFileExW")

func replaceFileAtomic(source, target string) error {
	s, _ := syscall.UTF16PtrFromString(source)
	t, _ := syscall.UTF16PtrFromString(target)
	r, _, e := moveFileEx.Call(uintptr(unsafe.Pointer(s)), uintptr(unsafe.Pointer(t)), uintptr(0x1|0x8))
	if r == 0 {
		return e
	}
	return nil
}
func syncDirectory(string) error { return nil }
