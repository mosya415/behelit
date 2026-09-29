//go:build unix

package main

import (
	"os"
	"strings"
	"testing"
)

// The line editor's history, Tab completion and "/" menu only exist in raw
// mode. This guards the build tags: if the unix implementation stops being
// compiled in (as happened when it was linux-only, leaving macOS on the cooked
// fallback), makeRaw returns the "unsupported" error instead of ENOTTY.
func TestRawModeCompiledOnUnix(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	_, err = makeRaw(int(r.Fd()))
	if err == nil {
		t.Fatal("makeRaw should fail on a pipe")
	}
	if strings.Contains(err.Error(), "unsupported") {
		t.Fatalf("the cooked fallback is compiled in on %s — no history or completion: %v", os.Getenv("GOOS"), err)
	}
}
