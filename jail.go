package main

import (
	"fmt"
	"path/filepath"
	"strings"
)

// Jail is the hard boundary (defense-in-depth alongside the soft approval gate).
// Every path a tool touches is realpath-resolved and must land inside Root;
// every command's argv[0] must be on the allowlist. Symlinks are followed before
// the prefix check, so a link pointing outside Root is rejected.
type Jail struct {
	Root    string   // absolute, symlink-resolved root
	Allowed []string // command allowlist, for display
	Unsafe  bool     // OFF the jail: any path, any command (see /unsafe)
	Shell   bool     // run commands through sh (pipes, redirects) — every segment still checked
	allowed map[string]bool
}

func NewJail(root string, allowed []string, unsafe bool) (*Jail, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, fmt.Errorf("root %q: %w", root, err)
	}
	set := map[string]bool{}
	for _, c := range allowed {
		set[c] = true
	}
	return &Jail{Root: real, Allowed: allowed, Unsafe: unsafe, allowed: set}, nil
}

// Resolve turns a tool-supplied path into an absolute path guaranteed to be
// inside the jail. It resolves symlinks on the deepest existing ancestor (the
// target itself may not exist yet, e.g. a new file being written).
func (j *Jail) Resolve(p string) (string, error) {
	if p == "" {
		return "", fmt.Errorf("empty path")
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(j.Root, p)
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	if j.Unsafe {
		return abs, nil // no confinement — any path
	}

	// Walk up to the nearest existing ancestor and resolve its symlinks, then
	// re-append the non-existent tail. This defeats symlink escapes on both the
	// target and any intermediate directory.
	real, tail := abs, ""
	for {
		if r, err := filepath.EvalSymlinks(real); err == nil {
			real = r
			break
		}
		parent := filepath.Dir(real)
		if parent == real {
			return "", fmt.Errorf("cannot resolve %q", p)
		}
		tail = filepath.Join(filepath.Base(real), tail)
		real = parent
	}
	resolved := real
	if tail != "" {
		resolved = filepath.Join(real, tail)
	}

	if !within(j.Root, resolved) {
		return "", fmt.Errorf("path %q escapes jail root %q", p, j.Root)
	}
	return resolved, nil
}

// AllowCommand reports whether argv[0]'s basename is on the allowlist (always
// true in unsafe mode).
func (j *Jail) AllowCommand(argv0 string) bool {
	if j.Unsafe {
		return true
	}
	return j.allowed[filepath.Base(argv0)]
}

// GPU policy: GPU work goes through the berserk scheduler (bsk submit), never
// straight onto a node — the agent must not grab GPUs out from under the
// queue. Enforced for every command, allowlisted or not, safe or unsafe mode.
var gpuLaunchers = map[string]bool{
	"srun": true, "sbatch": true, "salloc": true, "torchrun": true, "deepspeed": true,
	"accelerate": true, "mpirun": true, "mpiexec": true, "vllm": true, "trtllm-serve": true,
}

// gpuModules are python -m targets that start GPU serving or distributed runs.
var gpuModules = []string{"vllm", "sglang", "torch.distributed"}

// wrappers run their argument as a command; the policy looks through them.
var wrappers = map[string]bool{
	"nohup": true, "timeout": true, "nice": true, "ionice": true, "xargs": true, "exec": true,
	"time": true, "sudo": true, "stdbuf": true, "env": true, "command": true, "builtin": true,
	"setsid": true, "chrt": true, "taskset": true, "numactl": true, "then": true, "do": true, "else": true,
}

var shells = map[string]bool{"sh": true, "bash": true, "zsh": true, "dash": true, "ksh": true}

const gpuHint = "GPU jobs must go through the scheduler: bsk submit -g <gpus> [--name n] -- <command>"

// CheckCommand applies the sandbox to a command line: the allowlist (safe
// mode), the GPU policy and the bsk admin ban (always). In unsafe mode the
// line runs through sh, so every chained segment is checked.
func (j *Jail) CheckCommand(cmdline string) error {
	if j.Unsafe {
		return checkShellLine(nil, cmdline, 0)
	}
	if j.Shell {
		// sandbox: {shell: true} — the line goes to sh, so pipes and redirects
		// work, and the allowlist is applied to every segment of it instead of
		// to one argv.
		if strings.TrimSpace(cmdline) == "" {
			return fmt.Errorf("empty command")
		}
		return checkShellLine(j, cmdline, 0)
	}
	argv, err := tokenize(cmdline)
	if err != nil {
		return err
	}
	if len(argv) == 0 {
		return fmt.Errorf("empty command")
	}
	if !j.AllowCommand(argv[0]) {
		return fmt.Errorf("command %q is not on the allowlist", argv[0])
	}
	return checkArgv(nil, argv, 0)
}

// checkShellLine checks every segment of a shell line (split on operators,
// subshell/group brackets), with quoting removed per segment.
func checkShellLine(j *Jail, line string, depth int) error {
	if depth > 4 {
		return fmt.Errorf("command nesting too deep to verify")
	}
	for _, seg := range splitShellSegments(line) {
		argv, err := tokenize(strings.NewReplacer("\\", "").Replace(seg))
		if err != nil {
			argv = strings.Fields(seg)
		}
		if err := checkArgv(j, argv, depth); err != nil {
			return err
		}
	}
	return nil
}

// checkArgv checks one simple command: env assignments and wrappers are looked
// through, shells' -c strings are checked recursively, bsk may launch anything
// after "submit" (that is the scheduler path) but never "gw". With j non-nil
// (shell mode) the command this segment runs must also be on the allowlist.
func checkArgv(j *Jail, argv []string, depth int) error {
	i := 0
	for i < len(argv) {
		a := argv[i]
		name := filepath.Base(a)
		switch {
		case a == "export" || a == "declare" || a == "local" || a == "readonly":
			i++
		case shellNoop[a]:
			i++ // if/then/while/{ … — the command follows
		case shellData[a]:
			return nil // for/case/[[ … — a word list, not a command
		case strings.Contains(a, "=") && !strings.HasPrefix(a, "-"):
			key := a[:strings.IndexByte(a, '=')]
			if key == "CUDA_VISIBLE_DEVICES" || key == "NVIDIA_VISIBLE_DEVICES" {
				return fmt.Errorf("pinning GPUs directly is not allowed — %s", gpuHint)
			}
			i++
		case wrappers[name]:
			i++
			// skip the wrapper's own options and numeric/duration arguments
			for i < len(argv) && (strings.HasPrefix(argv[i], "-") || isWrapperArg(name, argv[i])) {
				if wrapperFlagTakesValue[name+" "+argv[i]] {
					i++ // the flag's value, e.g. env -u NAME, timeout -s KILL
				}
				i++
			}
		default:
			goto command
		}
	}
	return nil

command:
	name := filepath.Base(argv[i])
	rest := argv[i+1:]
	if gpuLaunchers[name] { // the GPU policy first: its message is the useful one
		return fmt.Errorf("%s launches GPU work directly — %s", name, gpuHint)
	}
	if j != nil && !j.AllowCommand(name) {
		return fmt.Errorf("command %q is not on the allowlist", name)
	}
	if shells[name] {
		for k, arg := range rest {
			if strings.HasPrefix(arg, "-") && strings.Contains(arg, "c") && k+1 < len(rest) {
				return checkShellLine(j, rest[k+1], depth+1)
			}
		}
	}
	// ssh/scp/rsync reach another machine: the command they carry is checked
	// too, so the GPU policy can't be sidestepped with `ssh node srun …`.
	if name == "ssh" || name == "rsh" {
		for k := 0; k < len(rest); k++ {
			arg := rest[k]
			if strings.HasPrefix(arg, "-") {
				if sshFlagTakesValue[arg] {
					k++ // its value is not the host
				}
				continue
			}
			if k+1 < len(rest) { // past the host: what it will run there
				return checkShellLine(nil, strings.Join(rest[k+1:], " "), depth+1)
			}
			return nil
		}
		return nil
	}
	if name == "bsk" {
		for _, arg := range rest {
			if strings.HasPrefix(arg, "-") {
				continue // global flags (and their values are not subcommands we care about)
			}
			switch arg {
			case "gw":
				return fmt.Errorf("bsk gw (gateway administration) is not available to agents")
			case "submit":
				return nil // the scheduler path: anything after it runs as a queued job
			}
		}
		return nil
	}
	if strings.HasPrefix(name, "python") {
		for k, arg := range rest {
			mod := ""
			switch {
			case arg == "-m" && k+1 < len(rest):
				mod = rest[k+1]
			case strings.HasPrefix(arg, "-m") && len(arg) > 2:
				mod = arg[2:]
			}
			for _, g := range gpuModules {
				if mod == g || strings.HasPrefix(mod, g+".") {
					return fmt.Errorf("python -m %s starts GPU serving/distributed work directly — %s", mod, gpuHint)
				}
			}
			if mod != "" {
				break
			}
		}
	}
	return nil
}

// wrapperFlagTakesValue lists wrapper options whose value is a separate word.
// ssh options whose value is a separate word, so it isn't taken for the host.
var sshFlagTakesValue = map[string]bool{
	"-o": true, "-p": true, "-i": true, "-l": true, "-F": true, "-J": true, "-L": true, "-R": true,
	"-D": true, "-b": true, "-c": true, "-E": true, "-e": true, "-I": true, "-m": true, "-O": true,
	"-Q": true, "-S": true, "-W": true, "-w": true, "-B": true,
}

var wrapperFlagTakesValue = map[string]bool{
	"env -u": true, "env --unset": true, "env -C": true, "env --chdir": true, "env -S": true,
	"timeout -s": true, "timeout --signal": true, "timeout -k": true, "timeout --kill-after": true,
	"nice -n": true, "nice --adjustment": true, "ionice -c": true, "ionice -n": true,
	"sudo -u": true, "sudo -g": true, "sudo -C": true, "stdbuf -i": true, "stdbuf -o": true, "stdbuf -e": true,
	"taskset -c": true, "numactl -C": true, "numactl -N": true, "numactl -m": true, "xargs -I": true,
	"xargs -n": true, "xargs -P": true, "xargs -d": true, "xargs -a": true, "xargs -L": true,
}

func isWrapperArg(wrapper, arg string) bool {
	switch wrapper {
	case "timeout", "nice", "chrt", "taskset":
		// durations, priorities, cpu masks: start with a digit
		return arg != "" && (arg[0] >= '0' && arg[0] <= '9')
	case "env":
		return strings.Contains(arg, "=") && !strings.HasPrefix(arg, "CUDA_VISIBLE_DEVICES=") && !strings.HasPrefix(arg, "NVIDIA_VISIBLE_DEVICES=")
	}
	return false
}

// Shell syntax that is not a command: keywords the command follows (shellNoop)
// and keywords introducing a word list (shellData). Without these, a segment
// like "for f in *.go" would be read as a command named "for".
var shellNoop = map[string]bool{
	"if": true, "elif": true, "while": true, "until": true, "!": true,
	"{": true, "}": true, "(": true, ")": true, "coproc": true,
}

var shellData = map[string]bool{
	"for": true, "select": true, "case": true, "in": true, "esac": true,
	"fi": true, "done": true, ";;": true, "[[": true, "[": true, "continue": true, "break": true,
	// shell builtins that run no program of their own
	"exit": true, "return": true, ":": true, "shift": true, "unset": true, "wait": true, "set": true, "trap": true,
}

// splitShellSegments cuts a shell line into simple commands at the operators
// (;, |, &&, ||, &, newlines, subshells and command substitutions), ignoring
// operators inside quotes — otherwise a quoted argument like 'a;b' would be
// read as a second command, and a legitimate awk program would be rejected.
func splitShellSegments(line string) []string {
	var segs []string
	var cur strings.Builder
	cut := func() {
		segs = append(segs, cur.String())
		cur.Reset()
	}
	const (
		plain = iota
		single
		double
	)
	state := plain
	for i := 0; i < len(line); i++ {
		c := line[i]
		switch state {
		case single:
			cur.WriteByte(c)
			if c == '\'' {
				state = plain
			}
			continue
		case double:
			cur.WriteByte(c)
			switch c {
			case '\\':
				if i+1 < len(line) {
					i++
					cur.WriteByte(line[i])
				}
			case '"':
				state = plain
			}
			continue
		}
		switch c {
		case '\\':
			cur.WriteByte(c)
			if i+1 < len(line) { // an escaped character is data, operator or not
				i++
				cur.WriteByte(line[i])
			}
		case '\'':
			state = single
			cur.WriteByte(c)
		case '"':
			state = double
			cur.WriteByte(c)
		case '&', '|':
			if i+1 < len(line) && line[i+1] == c {
				i++ // && or ||
			}
			cut()
		case ';', '\n', '(', ')', '{', '}', '`':
			cut()
		case '$':
			if i+1 < len(line) && line[i+1] == '(' { // command substitution
				i++
				cut()
				break
			}
			cur.WriteByte(c)
		default:
			cur.WriteByte(c)
		}
	}
	cut()
	return segs
}

func within(root, path string) bool {
	if path == root {
		return true
	}
	return strings.HasPrefix(path, root+string(filepath.Separator))
}
