//go:build darwin

package store

import (
	"strings"

	"golang.org/x/sys/unix"
)

// diskUsage reports free and total bytes for the filesystem holding path.
func diskUsage(path string) (free, total uint64, err error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return 0, 0, err
	}
	bs := uint64(st.Bsize)
	return st.Bavail * bs, st.Blocks * bs, nil
}

// networkFS reports whether path lives on a filesystem where SQLite's WAL mode is unsafe.
// Darwin is a development platform here, so this only catches the obvious mounts.
func networkFS(path string) (bool, string) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return false, ""
	}
	name := unix.ByteSliceToString(st.Fstypename[:])
	switch strings.ToLower(name) {
	case "nfs", "smbfs", "afpfs", "webdav":
		return true, name
	}
	return false, ""
}
