//go:build !unix

package main

// osTermWidth has no portable ioctl on non-unix platforms; fall back to
// $COLUMNS / default via the caller.
func osTermWidth() int { return 0 }
