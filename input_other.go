//go:build !unix

package main

import "os"

// readAvailable needs a non-blocking read, which this platform doesn't get:
// input typed while the agent works is read at the next prompt instead.
func readAvailable(f *os.File) []byte { return nil }
