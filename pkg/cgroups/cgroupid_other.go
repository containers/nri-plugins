//go:build !linux

package cgroups

func getID(path string) uint64 {
	panic("not implemented")
}
