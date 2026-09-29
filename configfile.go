package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

// The writer for config.json. fileconfig.go reads it; this writes it, and the
// two do not share a representation on purpose.
//
// It works on json.RawMessage and NOT on a decoded FileConfig, for two reasons:
// PermissionConfig decodes an ORDERED object because order is precedence (last
// match wins), so a decode/re-encode round trip would silently shuffle an
// operator's permission rules and change what the agent may do; and a newer
// binary's keys must survive an older binary's write.

// setConfigValues writes settings into one config.json, preserving everything it
// does not understand. A nil value deletes its key. It returns the path it wrote
// so the caller can name it on screen — a setting that persists invisibly is one
// nobody can find again.
//
// Two lca processes in one project therefore merge per key instead of clobbering
// per file. A genuine collision on the SAME key is last-writer-wins with no
// notification, which is the only sane semantics for a scalar.
func setConfigValues(path string, vals map[string]any) (string, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	unlock, err := lockConfig(dir)
	if err != nil {
		return "", err
	}
	defer unlock()
	return setConfigLocked(path, vals)
}

// setConfigLocked is setConfigValues with the lock already held, so /setup can
// write roles.yaml and config.json under ONE lock instead of two — a team file
// naming an endpoint the config never recorded is the half-written state the
// next start would trip over.
func setConfigLocked(path string, vals map[string]any) (string, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	old, readErr := os.ReadFile(path)
	obj := map[string]json.RawMessage{}
	if readErr == nil && len(bytes.TrimSpace(old)) > 0 {
		if err := json.Unmarshal(old, &obj); err != nil {
			return "", fmt.Errorf("%s: %w — fix or move it; refusing to overwrite a file we cannot read", path, err)
		}
	}
	for k, v := range vals {
		if v == nil {
			delete(obj, k)
			continue
		}
		b, err := json.Marshal(v)
		if err != nil {
			return "", fmt.Errorf("%s: %w", k, err)
		}
		obj[k] = b
	}
	data, err := marshalConfig(obj)
	if err != nil {
		return "", err
	}
	// mode 0600 as soon as a literal key is in the file, whatever put it there —
	// and also when the OLD bytes hold one, because the backup written just below
	// carries them and a key-bearing .bak is as readable as a key-bearing config.
	mode := os.FileMode(0o644)
	if _, ok := obj["api_key"]; ok || bytes.Contains(old, []byte(`"api_key"`)) {
		mode = 0o600
	}
	if readErr == nil { // keep the previous version next to it, as /role save does
		if err := writeFileMode(path+".bak", old, mode); err != nil {
			warnLine("could not keep a backup of %s: %v", filepath.Base(path), err)
		}
	}
	// pid AND a counter: two goroutines in one process share a pid, and a shared
	// temp name is two writers overwriting each other's bytes.
	tmp := fmt.Sprintf("%s.tmp.%d.%d", path, os.Getpid(), tmpSeq.Add(1))
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return "", err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(tmp)
		return "", err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return "", err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return "", err
	}
	if err := os.Chmod(tmp, mode); err != nil {
		os.Remove(tmp)
		return "", err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return "", err
	}
	return path, nil
}

// marshalConfig renders the object deterministically: the keys the setting table
// knows, in table order, then everything else sorted. A deterministic file is
// one whose `git diff` a reviewer can read.
func marshalConfig(obj map[string]json.RawMessage) ([]byte, error) {
	var order []string
	seen := map[string]bool{}
	for _, s := range settings {
		if s.JSON != "" && !seen[s.JSON] {
			seen[s.JSON] = true
			order = append(order, s.JSON)
		}
	}
	for _, k := range []string{"providers", "agents", "permission"} {
		if !seen[k] {
			seen[k] = true
			order = append(order, k)
		}
	}
	var rest []string
	for k := range obj {
		if !seen[k] {
			rest = append(rest, k)
		}
	}
	sort.Strings(rest)
	order = append(order, rest...)

	var b bytes.Buffer
	b.WriteString("{\n")
	n := 0
	for _, k := range order {
		raw, ok := obj[k]
		if !ok {
			continue
		}
		if n > 0 {
			b.WriteString(",\n")
		}
		n++
		key, _ := json.Marshal(k)
		b.WriteString("  ")
		b.Write(key)
		b.WriteString(": ")
		// Indent, not re-decode: a nested object's key order is precedence in the
		// permission block, and Indent moves whitespace only.
		var pretty bytes.Buffer
		if err := json.Indent(&pretty, bytes.TrimSpace(raw), "  ", "  "); err != nil {
			return nil, fmt.Errorf("%s: %w", k, err)
		}
		b.Write(pretty.Bytes())
	}
	if n > 0 {
		b.WriteString("\n")
	}
	b.WriteString("}\n")
	return b.Bytes(), nil
}

// writeFileMode writes data AT mode, even when the file already exists.
// os.WriteFile's perm argument applies only to a file it creates, so once a
// config.json.bak existed at 0644 from a key-free write, the next write copied a
// key-bearing config into it world-readable — 0600 on config.json, 0600 in the
// message on screen, and 0644 on the bytes lying next to it.
func writeFileMode(path string, data []byte, mode os.FileMode) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Chmod(path, mode)
}

var tmpSeq atomic.Uint64

// dirLocks serializes this process's own writers. The pid file below keeps OTHER
// processes out, and it cannot tell two goroutines of one process apart — they
// share a pid, so without this a /setup and a /set racing in one binary would
// each read the file before the other wrote it and the loser's key would vanish.
var dirLocks sync.Map // dir → *sync.Mutex

// lockDirs takes the lock for one or two directories at once, in path order so
// two processes cannot take them in opposite orders and wedge. Duplicates are
// collapsed, which is the normal case: roles.yaml and config.json usually live in
// the same .lca.
func lockDirs(dirs ...string) (func(), error) {
	seen := map[string]bool{}
	var want []string
	for _, d := range dirs {
		if d != "" && !seen[d] {
			seen[d] = true
			want = append(want, d)
		}
	}
	sort.Strings(want)
	var held []func()
	for _, d := range want {
		unlock, err := lockConfig(d)
		if err != nil {
			for i := len(held) - 1; i >= 0; i-- {
				held[i]()
			}
			return nil, err
		}
		held = append(held, unlock)
	}
	return func() {
		for i := len(held) - 1; i >= 0; i-- {
			held[i]()
		}
	}, nil
}

// lockConfig keeps two writers out of one read-modify-write. The cross-process
// half is copied from lockRun (workflow.go): a pid file, taken over when the pid
// is gone, because a crashed session must not leave a project unconfigurable.
// Held for milliseconds and NEVER across a prompt.
func lockConfig(dir string) (func(), error) {
	mu, _ := dirLocks.LoadOrStore(dir, &sync.Mutex{})
	m := mu.(*sync.Mutex)
	m.Lock()
	release := func(f func()) func() {
		return func() { f(); m.Unlock() }
	}
	path := filepath.Join(dir, "config.lock")
	for try := 0; try < 2; try++ {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			fmt.Fprintf(f, "%d\n", os.Getpid())
			f.Close()
			return release(func() { os.Remove(path) }), nil
		}
		if !os.IsExist(err) {
			m.Unlock()
			return nil, err
		}
		b, rerr := os.ReadFile(path)
		pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
		if rerr == nil && pid > 0 && pid != os.Getpid() && pidAlive(pid) {
			m.Unlock()
			return nil, fmt.Errorf("another lca (pid %d) is writing %s right now — try again in a moment", pid, dir)
		}
		if pid > 0 && pid != os.Getpid() {
			warnLine("taking over %s: its process (pid %d) is gone", filepath.Base(path), pid)
		}
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			m.Unlock()
			return nil, err
		}
	}
	m.Unlock()
	return nil, fmt.Errorf("%s: another process is opening it right now", dir)
}

// writeRoles writes rc to roles.yaml through the existing YAML() writer, keeping
// the previous file as roles.yaml.bak. Path: rc.Sources[0] when the team came
// from a file, else <root>/.lca/roles.yaml. It takes the same lock the config
// writer does, so /role save in one window and /setup in another serialize.
func writeRoles(root string, rc *RolesConfig, rec *Recorder) (string, error) {
	dir := filepath.Dir(rolesPath(root, rc))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	unlock, err := lockConfig(dir)
	if err != nil {
		return "", err
	}
	defer unlock()
	return writeRolesLocked(root, rc, rec)
}

// rolesPath is where this team's file lives: where it was read from, else the
// project's .lca/roles.yaml.
func rolesPath(root string, rc *RolesConfig) string {
	if rc != nil && len(rc.Sources) > 0 {
		return rc.Sources[0]
	}
	return filepath.Join(root, ".lca", "roles.yaml")
}

func writeRolesLocked(root string, rc *RolesConfig, rec *Recorder) (string, error) {
	path := rolesPath(root, rc)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	if old, err := os.ReadFile(path); err == nil { // keep the previous version next to it
		if err := writeFileMode(path+".bak", old, 0o644); err != nil {
			warnLine("could not keep a backup: %v", err)
		}
	}
	if err := os.WriteFile(path, []byte(rc.YAML()), 0o644); err != nil {
		return "", err
	}
	if rec != nil {
		rec.Event("roles_saved", map[string]any{"path": path})
	}
	return path, nil
}
