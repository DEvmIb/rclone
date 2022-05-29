//go:build !windows
// +build !windows

package kodi

import "os"

// Removes name, retrying on a sharing violation
func remove(name string) error {
	return os.Remove(name)
}
