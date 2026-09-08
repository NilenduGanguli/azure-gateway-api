//go:build darwin

package store

import (
	"strings"
	"syscall"
)

// diskUsage reports free and total bytes for the filesystem holding path.
func diskUsage(path string) (free, total uint64, err error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, 0, err
	}
	bs := uint64(st.Bsize)
	return st.Bavail * bs, st.Blocks * bs, nil
}

// networkFS reports whether path lives on a filesystem where SQLite's WAL mode is unsafe.
// Darwin is a development platform here, so this only needs to catch the obvious mounts.
func networkFS(path string) (bool, string) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return false, ""
	}
	name := cString(st.Fstypename[:])
	switch strings.ToLower(name) {
	case "nfs", "smbfs", "afpfs", "webdav":
		return true, name
	}
	return false, ""
}

// cString converts a NUL-terminated fixed-size C character array to a Go string.
func cString(b []int8) string {
	out := make([]byte, 0, len(b))
	for _, c := range b {
		if c == 0 {
			break
		}
		out = append(out, byte(c))
	}
	return string(out)
}
