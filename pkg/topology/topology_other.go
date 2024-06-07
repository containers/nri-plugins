//go:build !linux
// +build !linux

package topology

import "errors"

// FindSysFsDevice for given argument returns physical device where it is linked to.
// For device nodes it will return path for device itself. For regular files or directories
// this function returns physical device where this inode resides (storage device).
// If result device is a virtual one (e.g. tmpfs), error will be returned.
// For non-existing path, no error returned and path is empty.
func FindSysFsDevice(dev string) (string, error) {
	return "", errors.New("not implemented")
}
