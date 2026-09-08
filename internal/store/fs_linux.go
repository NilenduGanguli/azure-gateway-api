//go:build linux

package store

import "syscall"

// diskUsage reports free and total bytes for the filesystem holding path.
func diskUsage(path string) (free, total uint64, err error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, 0, err
	}
	bs := uint64(st.Bsize)
	return st.Bavail * bs, st.Blocks * bs, nil
}

// Filesystem magic numbers for the network filesystems where SQLite's WAL mode is unsafe: WAL
// depends on shared memory and POSIX advisory locks that these do not provide reliably.
// https://www.sqlite.org/wal.html#noshm
const (
	magicNFS   = 0x6969
	magicSMB   = 0x517B
	magicCIFS  = 0xFF534D42
	magicCEPH  = 0x00C36400
	magicFUSE  = 0x65735546
	magicGFS2  = 0x01161970
	magicOCFS2 = 0x7461636F
)

// networkFS reports whether path lives on a filesystem where WAL cannot be trusted.
func networkFS(path string) (bool, string) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return false, ""
	}
	switch int64(st.Type) {
	case magicNFS:
		return true, "nfs"
	case magicSMB:
		return true, "smb"
	case magicCIFS:
		return true, "cifs"
	case magicCEPH:
		return true, "ceph"
	case magicFUSE:
		return true, "fuse"
	case magicGFS2:
		return true, "gfs2"
	case magicOCFS2:
		return true, "ocfs2"
	}
	return false, ""
}
