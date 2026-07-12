//go:build !windows

package main

import "os"

func replaceFileAtomic(source, target string) error { return os.Rename(source, target) }
func syncDirectory(path string) error {
	d, err := os.Open(path)
	if err != nil {
		return err
	}
	if err = d.Sync(); err != nil {
		d.Close()
		return err
	}
	return d.Close()
}
