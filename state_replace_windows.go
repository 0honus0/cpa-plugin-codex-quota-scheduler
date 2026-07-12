//go:build windows

package main

import (
	"syscall"
	"unsafe"
)

var moveFileEx = syscall.NewLazyDLL("kernel32.dll").NewProc("MoveFileExW")

func replaceFileAtomic(source, target string) error {
	s, err := syscall.UTF16PtrFromString(source)
	if err != nil {
		return err
	}
	t, err := syscall.UTF16PtrFromString(target)
	if err != nil {
		return err
	}
	r, _, e := moveFileEx.Call(uintptr(unsafe.Pointer(s)), uintptr(unsafe.Pointer(t)), uintptr(0x1|0x8))
	if r == 0 {
		return e
	}
	return nil
}
func syncDirectory(string) error { return nil }
