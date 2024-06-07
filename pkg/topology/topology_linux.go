package topology

import (
	"fmt"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

// FindSysFsDevice for given argument returns physical device where it is linked to.
// For device nodes it will return path for device itself. For regular files or directories
// this function returns physical device where this inode resides (storage device).
// If result device is a virtual one (e.g. tmpfs), error will be returned.
// For non-existing path, no error returned and path is empty.
func FindSysFsDevice(dev string) (string, error) {
	fi, err := os.Stat(dev)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", fmt.Errorf("unable to get stat for %s: %w", dev, err)
	}

	devType := "block"
	rdev := fi.Sys().(*syscall.Stat_t).Dev
	if mode := fi.Mode(); mode&os.ModeDevice != 0 {
		rdev = fi.Sys().(*syscall.Stat_t).Rdev
		if mode&os.ModeCharDevice != 0 {
			devType = "char"
		}
	}

	major := int64(unix.Major(rdev))
	minor := int64(unix.Minor(rdev))
	if major == 0 {
		return "", fmt.Errorf("%s is a virtual device node: %w", dev, err)
	}

	realDevPath, err := findSysFsDevice(devType, major, minor)
	if err != nil {
		return "", fmt.Errorf("failed to find sysfs device for %s: %w", dev, err)
	}

	return realDevPath, nil
}
