package main

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// The apply layer. A search block is matched against the file exactly first;
// only if that fails do the tolerant strategies in replacers.go run, and every
// strategy must land on a single literal location (or replace_all). Zero or
// ambiguous matches are reported back to the model as an error so it
// regenerates — never a silent pick of one of several places.

// applyEdit performs a single search/replace on the file at path. `name` is the
// display path (as the model wrote it) used in the result message; IO is done
// on the jail-resolved `path`. The second result is the file's new content, so
// the caller can record its fingerprint without re-reading (and racing).
func applyEdit(path, name, search, replace string) (string, string, error) {
	return applyEditMode(path, name, search, replace, false)
}

// applyEditMode is applyEdit with replace_all. Line endings follow the file:
// a CRLF file gets CRLF in both search and replacement. It re-reads the file
// here, under the caller's lease, which is why `edit` has always survived a
// rival writer by accident — fuzzyReplace errors when the search text is gone.
func applyEditMode(path, name, search, replace string, replaceAll bool) (string, string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", "", fmt.Errorf("read %s: %w", name, err)
	}
	content := string(data)
	if strings.Contains(content, "\r\n") {
		search = toCRLF(search)
		replace = toCRLF(replace)
	}

	updated, strategy, err := fuzzyReplace(content, search, replace, replaceAll)
	if err != nil {
		return "", "", fmt.Errorf("%s: %w", name, err)
	}
	if updated == content {
		return "", "", fmt.Errorf("%s: the edit produced no change", name)
	}
	if err := writeFileAtomic(path, []byte(updated)); err != nil {
		return "", "", fmt.Errorf("write %s: %w", name, err)
	}
	msg := fmt.Sprintf("edited %s", name)
	if replaceAll {
		msg += " (all occurrences)"
	} else {
		msg += " (1 replacement)"
	}
	if strategy != "exact" {
		msg += " — matched via " + strategy + " fallback; re-read before further edits nearby"
	}
	return msg, updated, nil
}

func toCRLF(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\n", "\r\n")
}

// writeWholeFile replaces (or creates) a file with the given content, creating
// any missing parent directories along the way. Intended for small files where
// a full rewrite is clearer than a search/replace. `path` is already
// jail-resolved by the caller (so created directories stay inside the jail);
// `name` is the display path used in the result message.
//
// With excl the file must NOT exist: the caller decided this was a create, and a
// file that appeared in the meantime belongs to someone else. errFileExists is
// how it says so, because overwriting there is precisely the silent loss the
// create path used to allow (it skipped the staleness check altogether).
func writeWholeFile(path, name, content string, excl bool) (string, error) {
	dir := filepath.Dir(path)
	createdDir := false
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", fmt.Errorf("create dir for %s: %w", name, err)
		}
		createdDir = true
	}
	write := writeFileAtomic
	if excl {
		write = createFileAtomic
	}
	if err := write(path, []byte(content)); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return "", err // the caller phrases the create race; it knows the session
		}
		return "", fmt.Errorf("write %s: %w", name, err)
	}
	msg := fmt.Sprintf("wrote %s (%d bytes)", name, len(content))
	if createdDir {
		msg += ", created parent directory"
	}
	return msg, nil
}

// writeFileAtomic writes data through a temp file in the SAME directory and
// renames it over the target. Same directory means same filesystem means the
// rename is atomic, so no reader — another agent, the user's editor, a test
// runner — ever sees a half-written file. os.WriteFile truncates first and
// streams, which is a window of arbitrary length holding a file that is neither
// the old version nor the new one.
//
// Two details are not optional. os.WriteFile's perm argument applies only to a
// file it CREATES, so today's mode survives an overwrite by accident; os.Rename
// preserves nothing, so the mode has to be copied explicitly or an executable
// script comes back 0644. And if path is a symlink the rename would replace the
// LINK with a regular file, so the link is resolved and its target written.
//
// The cost, stated plainly: the inode changes. Hard links break, an editor or
// `tail -f` holding the old inode keeps seeing the old bytes, and extended
// attributes (a quarantine flag, an SELinux label) do not come across. That is
// the price of never serving half a file. Ownership and the mode bits ABOVE the
// low nine are not on that list — they are carried across deliberately below,
// because losing them silently is a change to the user's repository that nothing
// afterwards can see.
func writeFileAtomic(path string, data []byte) error {
	target := path
	if real, err := filepath.EvalSymlinks(path); err == nil {
		target = real
	}
	mode := fs.FileMode(0o644)
	uid, gid, haveOwner := 0, 0, false
	if info, err := os.Stat(target); err == nil && info.Mode().IsRegular() {
		// A file the user or a build step made unwritable is hands-off: that is what
		// `chmod 444` MEANS, and os.WriteFile refused it with EACCES. A rename needs
		// only the directory, so without this check the agent overwrites a vendored
		// artifact, a generated file or a checked-in read-only config invisibly — the
		// file comes back still 0444, holding something else.
		if err := refuseUnwritable(target); err != nil {
			return err
		}
		// The FULL mode and not just Perm(): setuid, setgid and the sticky bit sit
		// above the low nine, and a setgid helper script that comes back without its
		// setgid bit stops working for its group with nothing to explain why.
		mode = info.Mode() & (fs.ModePerm | fs.ModeSetuid | fs.ModeSetgid | fs.ModeSticky)
		uid, gid, haveOwner = fileOwner(info)
	}
	tmp, err := tempBeside(target, data, mode)
	if err != nil {
		return err
	}
	if err := os.Rename(tmp, target); err != nil {
		os.Remove(tmp)
		return err
	}
	// Chmod AFTER the rename as well as before it: on some systems a rename clears
	// setuid and setgid, and an fd's Chmod before the rename cannot carry them.
	if mode&(fs.ModeSetuid|fs.ModeSetgid|fs.ModeSticky) != 0 {
		os.Chmod(target, mode)
	}
	if haveOwner && !sameOwnerAsUs(uid, gid) {
		// Best effort: changing a file's uid needs root on Linux, so on a shared
		// repository this usually fails — and then the operator is TOLD, because the
		// consequence (a teammate can no longer write their own file) is otherwise
		// discovered days later and traced to nothing.
		if err := os.Chown(target, uid, gid); err != nil {
			warnLine("%s now belongs to this process instead of uid %d: an atomic write replaces the file, and restoring its owner needs privileges this process does not have", filepath.Base(target), uid)
		}
	}
	syncDir(filepath.Dir(target))
	return nil
}

// refuseUnwritable gives back the EACCES os.WriteFile used to give. Asking the
// kernel (O_WRONLY on the existing file) rather than reading the mode bits,
// because the answer depends on the uid, the gid, ACLs and a read-only mount,
// and only one of those is in the mode.
func refuseUnwritable(target string) error {
	f, err := os.OpenFile(target, os.O_WRONLY, 0)
	if err != nil {
		if errors.Is(err, fs.ErrPermission) {
			// The bare error, without the path: every caller wraps this with the
			// DISPLAY path, and every other guard message names the file the way the
			// model wrote it rather than leaking the jail-absolute one.
			return fs.ErrPermission
		}
		return err
	}
	return f.Close()
}

// syncDir flushes a directory entry so the rename itself survives a crash. Best
// effort: some filesystems refuse to open a directory for anything at all, and a
// rename that is atomic but not yet durable is still strictly better than a
// truncating write.
func syncDir(dir string) {
	if d, err := os.Open(dir); err == nil {
		d.Sync()
		d.Close()
	}
}

// createFileAtomic is writeFileAtomic for a file that must not exist yet. The
// complete temp file is HARD LINKED into place: link(2) fails with EEXIST if the
// name is taken, so the create either lands whole or loses the race — there is
// never a moment where the name exists holding zero bytes, which an O_EXCL
// claim followed by a write would leave open to a reader. Where links are not
// available the O_EXCL claim is the fallback, with that small window admitted.
func createFileAtomic(path string, data []byte) error {
	mode := fs.FileMode(0o644)
	tmp, err := tempBeside(path, data, mode)
	if err != nil {
		return err
	}
	defer os.Remove(tmp)
	if err := os.Link(tmp, path); err == nil {
		syncDir(filepath.Dir(path))
		return nil
	} else if errors.Is(err, fs.ErrExist) {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	f.Close()
	return os.Rename(tmp, path)
}

// tempBeside writes data to a fresh file in path's directory, with the given
// mode, and fsyncs it. Beside, and not in TMPDIR, so the rename that follows
// cannot cross a filesystem boundary (which would make it a copy, and not
// atomic).
//
// The Sync is not decoration. On a filesystem that commits metadata before data
// — ext4 with delayed allocation, and it is not alone — a power loss or kernel
// panic shortly after the rename leaves the directory entry pointing at the new
// inode with zero or partial length in it: the user's original bytes are gone
// and the replacement is empty. os.WriteFile was not durable either, but it did
// not stake the OLD content on the new bytes reaching the platter. This is the
// one writer the design promises never serves half a file, so it pays for it:
// measured at 8.2 ms a write against 130 µs without, on an APFS SSD — once per
// `edit` or `write` tool call and nowhere else in the program.
func tempBeside(path string, data []byte, mode fs.FileMode) (string, error) {
	return tempBesideWritten(path, mode, func(w io.Writer) error {
		_, err := w.Write(data)
		return err
	})
}

// tempBesideString is tempBeside for data that is already a string. It exists
// for one caller: the check log, which on a stand is the whole of a five-minute
// deploy's output. []byte(s) there copies every byte of it — measured at 500 MB
// of garbage per attempt, on a peak that already held the same output three
// times over — and io.WriteString hands the string straight to the file.
func tempBesideString(path, data string, mode fs.FileMode) (string, error) {
	return tempBesideWritten(path, mode, func(w io.Writer) error {
		_, err := io.WriteString(w, data)
		return err
	})
}

func tempBesideWritten(path string, mode fs.FileMode, write func(io.Writer) error) (string, error) {
	f, err := os.CreateTemp(filepath.Dir(path), ".lca-tmp-")
	if err != nil {
		return "", err
	}
	name := f.Name()
	fail := func(err error) (string, error) {
		f.Close()
		os.Remove(name)
		return "", err
	}
	if err := write(f); err != nil {
		return fail(err)
	}
	// CreateTemp makes the file 0600; the mode has to be set before the rename,
	// because after it the name belongs to a file nobody is holding open.
	if err := f.Chmod(mode); err != nil {
		return fail(err)
	}
	if err := f.Sync(); err != nil {
		return fail(err)
	}
	if err := f.Close(); err != nil {
		os.Remove(name)
		return "", err
	}
	return name, nil
}

// unifiedPreview renders a minimal, human-readable diff for the approval prompt.
// It is display-only — not fed to the model and not used to apply anything.
func unifiedPreview(search, replace string) string {
	var b strings.Builder
	for _, ln := range strings.Split(search, "\n") {
		b.WriteString("  - " + ln + "\n")
	}
	for _, ln := range strings.Split(replace, "\n") {
		b.WriteString("  + " + ln + "\n")
	}
	return b.String()
}
