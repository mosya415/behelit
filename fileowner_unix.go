//go:build unix

package main

import (
	"io/fs"
	"os"
	"syscall"
)

// Who owns a file, for the one thing an atomic write cannot preserve.
//
// writeFileAtomic renames a new inode over the target, and a new inode belongs
// to the process that made it. os.WriteFile kept ownership for free because it
// kept the inode. On a shared repository — a file a teammate owns in a
// group-writable directory — that difference is invisible afterwards and costs
// the teammate their write access, so it is restored where the kernel allows it
// and SAID where it does not. Silently reassigning ownership is exactly the kind
// of unreported change this guard exists to stop.
//
// Split by build tag the way width_unix.go/width_other.go already are: Uid and
// Gid live on syscall.Stat_t, which only unix has.

func fileOwner(info fs.FileInfo) (uid, gid int, ok bool) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, false
	}
	return int(st.Uid), int(st.Gid), true
}

func sameOwnerAsUs(uid, gid int) bool { return uid == os.Getuid() && gid == os.Getgid() }
