//go:build !unix

package main

import "io/fs"

// Nowhere to read a uid from, so the atomic write makes no claim about ownership
// — the same shape width_other.go takes for the same reason. ok=false makes
// every caller skip the restore rather than guess at one.

func fileOwner(fs.FileInfo) (uid, gid int, ok bool) { return 0, 0, false }

func sameOwnerAsUs(int, int) bool { return true }
