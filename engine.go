package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// The engine: an Orchestrator owns everything shared (jail, approval gate,
// audit log, providers, agents, skills, commands, the subagent registry), and a
// Session is one conversation driven by one agent on one model. The REPL owns a
// primary session; the task tool creates child sessions that run the very same
// loop headlessly — concurrently, on their own models — and report back. The
// loop knows nothing about the terminal: everything it shows goes through a
// View (view.go), which is the seam for other front-ends (JSON events, a server).

type Orchestrator struct {
	cfg       Config
	fc        *FileConfig
	jl        *Jail
	ap        *Approver
	rec       *Recorder
	providers *Providers
	agents    map[string]*Agent
	skills    map[string]*Skill
	commands  map[string]*Command
	userRules Ruleset
	warnings  []string

	mu       sync.Mutex
	children map[string]*Session // subagent sessions by task id (resumable)
	nextTask int
	slots    chan struct{} // bounds concurrently running subagents

	// locks is where cross-process file leases live (filelock.go): the git common
	// dir of the project, resolved once. Not a config-derived field — two
	// Orchestrators with different LCA_DIRs on one repository must land on the
	// same directory, which is the whole point of asking git for it.
	locksOnce sync.Once
	locks     string

	// Whether this git can merge in the object database (branch.go), asked once:
	// the answer is a property of the binary and cannot change while we run, and
	// every integration would otherwise pay a subprocess to re-learn it.
	mt3Once sync.Once
	mt3     bool
	mt3Ver  string

	// The integration wave, for "integrated 2nd of 3". Parallel results are never
	// buffered into reply order — that would make a finished delegation wait on a
	// slower sibling and only widen the window for the human to touch the file —
	// so the ordinal is all the caller gets, and it has to be counted somewhere
	// that outlives the individual delegations. The wave restarts once no
	// delegation is in flight, so a later pair is "1st of 2" again and not
	// "7th of 8".
	waveMu       sync.Mutex
	waveInflight int
	waveStarted  int
	waveDone     int

	// mcp is the configured internal MCP servers, their reachability allowlist and
	// the pinned tool manifest. nil is impossible after buildOrchestrator; empty is
	// the normal case.
	mcp *MCPSet

	roles     *RolesConfig // roles.yaml, nil when absent
	remote    *Remote      // the "remote" member: the old single-machine spelling
	tracer    *Tracer
	worktrees worktrees

	// The fleet (members.go). members always holds "local"; the remote: member
	// is synthesised from o.remote on demand instead of copied here, because
	// o.remote is assigned after construction in more than one place.
	members    map[string]*Member
	memOrd     []string // declaration order
	defMem     string   // defaults: member:
	hasMembers bool     // a members: block was declared

	memMu  sync.Mutex
	locMem *Member // the local member, allocated once
	legMem *Member // the synthesised "remote" member, keyed on o.remote

	gatewayModels int // models the gateway lists: -1 not checked, 0 unreachable

	histMu  sync.Mutex
	history []*taskEntry // every subagent run, for /tasks
}

// taskEntry is one subagent run as /tasks shows it.
type taskEntry struct {
	ID, Kind, Agent, Title, Status, Detail string
	Start                                  time.Time
	Duration                               time.Duration
}

func (o *Orchestrator) trackStart(id, kind, agent, title string) *taskEntry {
	e := &taskEntry{ID: id, Kind: kind, Agent: agent, Title: title, Status: "running", Start: time.Now()}
	o.histMu.Lock()
	o.history = append(o.history, e)
	o.histMu.Unlock()
	return e
}

func (o *Orchestrator) trackEnd(e *taskEntry, status, detail string) {
	o.histMu.Lock()
	e.Status, e.Detail, e.Duration = status, detail, time.Since(e.Start)
	o.histMu.Unlock()
}

// History returns a snapshot of subagent runs, oldest first.
func (o *Orchestrator) History() []taskEntry {
	o.histMu.Lock()
	defer o.histMu.Unlock()
	out := make([]taskEntry, len(o.history))
	for i, e := range o.history {
		out[i] = *e
	}
	return out
}

func NewOrchestrator(cfg Config, fc *FileConfig, jl *Jail, ap *Approver, rec *Recorder, local *Client, roles *RolesConfig, tracer *Tracer) *Orchestrator {
	if roles != nil && roles.Transport != "" {
		local.provider.Transport = roles.Transport // the gateway is the local endpoint
	}
	ps := NewProviders(cfg, fc, local)
	ps.UseStateDir(cfg.stateDir()) // remembered context windows outlive the session
	o := &Orchestrator{cfg: cfg, fc: fc, jl: jl, ap: ap, rec: rec, providers: ps, roles: roles, tracer: tracer, gatewayModels: -1,
		children: map[string]*Session{},
		slots:    make(chan struct{}, atoiDefault(os.Getenv("LCA_MAX_PARALLEL"), 4))}
	var w []string
	o.agents, w = loadAgents(jl.Root, cfg.Dir, fc, ps)
	o.warnings = append(o.warnings, w...)
	if roles != nil {
		for _, r := range roles.Roles {
			o.agents[r.Name] = r // a role replaces a same-named agent
		}
	}
	o.setFleet(roles)
	o.skills = loadSkills(jl.Root, cfg.Dir)
	o.commands, w = loadCommands(jl.Root, cfg.Dir, ps)
	o.warnings = append(o.warnings, w...)
	if fc != nil {
		o.userRules = Ruleset(fc.Permission)
	}
	return o
}

func (o *Orchestrator) subagentDepth() int {
	if o.fc != nil && o.fc.SubagentDepth > 0 {
		return o.fc.SubagentDepth
	}
	return max(o.cfg.SubagentMax, 1)
}

// subagentsFor lists the agents s may delegate to (subagent mode, not hidden,
// not denied by its task rules), sorted by name.
func (o *Orchestrator) subagentsFor(s *Session) []*Agent {
	var out []*Agent
	for _, a := range o.agents {
		if a.isSubagent() && !a.Hidden && Evaluate("task", a.Name, s.rules()...) != Deny {
			out = append(out, a)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// primaryAgents lists agents selectable with /agent.
func (o *Orchestrator) primaryAgents() []*Agent {
	var out []*Agent
	for _, a := range o.agents {
		if a.isPrimary() && !a.Hidden {
			out = append(out, a)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// seenFile is what a session was SHOWN of a file, and when. mtime alone was not
// enough three different ways: a whole second of mtime granularity hides a
// change (and an ssh `stat` has only seconds), a same-size overwrite inside that
// second is invisible, and a `touch` or a `gofmt -w` that changed nothing looked
// like someone else's edit and sent the model back to re-read an identical file.
type seenFile struct {
	mtime time.Time // the file's mtime at the moment it was shown
	size  int64
	sum   string // hex sha256 of the bytes the model was SHOWN; "" when not a whole read
	// noisy says the bytes the model was shown are NOT the file's bytes, and that
	// the FILE is not to blame: on a member whose non-interactive shell prints a
	// banner, `shown` is banner+content while the member's own sha256 is of
	// content alone. The record then holds the MEMBER's hash — which is what its
	// next compare-and-swap compares against, so it is the authoritative one — and
	// the refusal names the rc file rather than the file. Reported as staleness
	// instead it named the wrong cause and offered a remedy that cannot work: a
	// re-read reproduces the same banner, so remote editing stayed wedged for ever
	// behind a message about a file nobody had touched.
	noisy bool
	at    time.Time // when it was shown — the only thing that can say "you read it at"
}

// readSet is one session's file knowledge. Per SESSION, not per process: the map
// used to hang off the Orchestrator, so a `task` child could edit a file only its
// parent had read, and the child's post-write note refreshed the PARENT's record
// — which then licensed the parent's next whole-file write to erase the child's
// work. The task tool's own description promises "the subagent starts with none
// of your context"; this is that promise kept.
type readSet struct {
	mu sync.Mutex
	m  map[string]seenFile
}

func (rs *readSet) note(key string, sf seenFile) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if rs.m == nil {
		rs.m = map[string]seenFile{}
	}
	rs.m[key] = sf
}

func (rs *readSet) get(key string) (seenFile, bool) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	sf, ok := rs.m[key]
	return sf, ok
}

func (rs *readSet) forget(key string) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	delete(rs.m, key)
}

// Session is one conversation: agent + model + transcript.
type Session struct {
	ID     string // short display id: "main", "t1", …
	UID    string // globally unique: x-session-id, trace, transcripts
	orch   *Orchestrator
	parent *Session
	depth  int
	agent  *Agent
	client *Client
	view   View
	Msgs   []Message
	extra  Ruleset // session-level restrictions (subagents)

	checkLive io.Writer // where a verifier check's output streams live (nil: nowhere)
	member    string    // member NAME pinned to this session ("" = resolve from the role)
	wtRem     *Remote   // a delegate worktree that lives on a member, not on this machine
	jl        *Jail     // own working tree (delegate worktree); nil = the orchestrator's
	isolated  bool      // works in a scratch worktree: edits/commands there are the point
	// branch and wt are set only under `apply: branch`: the real branch this
	// session's work is committed onto, and the worktree holding it. Both empty is
	// today's behaviour to the byte — commitWork is a no-op and nothing else in the
	// loop looks at them.
	branch       string
	wt           *worktree
	rootOverride string   // root session id for detached helper sessions (compaction)
	transport    string   // tool transport, fixed per session ("" = the client's provider)
	malformed    int      // text-protocol tool tags in the last reply that didn't parse
	models       []string // role model chain (gateway names); empty = fixed client
	modelIdx     int
	schemas      []ToolSchema // tool schemas, computed once: the request prefix never changes
	toolDefs     []*ToolDef

	Loop      bool // primary: autonomous loop mode (/loop)
	ShowThink bool
	Raw       bool
	TTY       bool // Ctrl-C interrupts the model (interactive primary)

	LastReason string // reasoning captured in the last run (/think last)
	title      string // subagent: the task description

	mu      sync.Mutex
	rs      *readSet // files THIS session has been shown (lazily made; readSet())
	lastCmd struct { // the last run_command / check_cmd, for blame
		line string
		at   time.Time
	}
	todos             []Todo
	inbox             []string // results of finished background subagents
	bgRunning         int
	wake              chan struct{}
	recent            []string // signatures of recent tool calls (doom-loop detection)
	stats             SessionStats
	running           atomic.Bool
	interruptedAtDoor atomic.Bool // Ctrl-C answered an approval question
	background        bool        // runs detached from the terminal: can't prompt for approval
	lastUsage         Usage
}

// NewPrimary creates the REPL / one-shot session.
func (o *Orchestrator) NewPrimary(agentName, modelRef string, view View) (*Session, error) {
	ag := o.agents[agentName]
	if ag == nil {
		return nil, fmt.Errorf("unknown agent %q", agentName)
	}
	s := &Session{ID: "main", UID: o.rec.id, orch: o, agent: ag, view: view, wake: make(chan struct{}, 1)}
	if len(ag.Models) > 0 && modelRef == "" {
		s.models = append([]string(nil), ag.Models...)
		s.useModel(0)
	} else if err := s.SetModel(firstNonEmpty(modelRef, ag.Model)); err != nil {
		return nil, err
	}
	s.Msgs = []Message{{Role: "system", Content: s.systemPrompt()}}
	return s, nil
}

func firstNonEmpty(xs ...string) string {
	for _, x := range xs {
		if x != "" {
			return x
		}
	}
	return ""
}

// SetModel points the session at a model ref ("" = the local endpoint client).
func (s *Session) SetModel(ref string) error {
	c, err := s.orch.providers.Client(ref)
	if err != nil {
		return err
	}
	s.client = c
	s.models, s.modelIdx = nil, 0 // an explicit model replaces the role chain
	return nil
}

func (s *Session) Client() *Client { return s.client }
func (s *Session) Agent() *Agent   { return s.agent }

// remote is the machine this session's files and commands live on, or nil for
// local work. The contract is unchanged to the letter: nil means "this
// machine", and a delegate worktree ON this machine stays nil however its role
// is pinned (s.jl is that worktree's jail). A worktree on a MEMBER carries its
// own transport, because it is a different directory on that machine.
func (s *Session) remote() *Remote {
	if s.wtRem != nil {
		return s.wtRem
	}
	if s.jl != nil {
		return nil
	}
	return s.memberOf().Rem
}

// applyModelOpts fills in what the model card says for the model in use, for
// anything the role didn't set explicitly.
func (s *Session) applyModelOpts(req *ChatRequest) {
	if s.orch.roles == nil {
		return
	}
	o := s.orch.roles.modelOpts(s.client.Model())
	if o == nil {
		return
	}
	if req.Temperature == nil {
		req.Temperature = o.Temperature
	}
	if req.TopP == nil {
		req.TopP = o.TopP
	}
	if req.TopK == nil && o.TopK > 0 {
		k := o.TopK
		req.TopK = &k
	}
	if req.Thinking == "" {
		req.Thinking = o.Effort
	}
}

// sampling describes what this session actually sends, and where each value came
// from, for /model and doctor. The precedence is body()'s: the role, then
// roles.yaml models.<id>, then the profile, then nothing at all — and "unset" is
// a real answer, not a missing one.
func (s *Session) sampling() (temp, topP, effort string) {
	prof := s.client.Profile()
	var o *ModelOpts
	if s.orch.roles != nil {
		o = s.orch.roles.modelOpts(s.client.Model())
	}
	var optTemp, optTopP *float64
	optEffort := ""
	if o != nil {
		optTemp, optTopP, optEffort = o.Temperature, o.TopP, o.Effort
	}
	effort = firstNonEmpty(s.thinking(), optEffort)
	switch {
	case effort == "":
		effort = "provider default"
	case len(prof.Efforts) > 0 && s.client.EffortFor(effort) == "":
		// Named but not accepted: chat.go drops it, so say so here rather than
		// letting /model imply a level the model will never see. The vocabulary
		// quoted is the one this endpoint checks against — a self-hosted template
		// resolves fewer spellings than the vendor's API does.
		effort += faint(" (not sent — %s accepts %s)", prof.Key, strings.Join(s.client.EffortVocab(), "|"))
	case prof.EffortNone && prof.Switch.Kwarg != "":
		// The LEVEL is dropped, but the card's own switch still goes: saying
		// "not sent" here read as "thinking was not asked for", which is the
		// opposite of what the request does.
		effort += faint(" (level not sent — %s documents no effort field; thinking is switched on with %s)", prof.Key, prof.Switch.Kwarg)
	case prof.EffortNone:
		effort += faint(" (not sent — %s documents no effort field)", prof.Key)
	}
	return sampleSrc(s.agent.Temperature, optTemp, prof.Temperature, prof.Src.Temperature),
		sampleSrc(s.agent.TopP, optTopP, prof.TopP, prof.Src.TopP), effort
}

// sampleSrc renders one sampling value with its provenance, so an operator can
// tell "1.0 (card)" from "1.0 (role)" from a number nobody sourced.
func sampleSrc(role, opt, prof *float64, src Origin) string {
	switch {
	case role != nil:
		return strconv.FormatFloat(*role, 'f', -1, 64) + " (role)"
	case opt != nil:
		return strconv.FormatFloat(*opt, 'f', -1, 64) + " (roles.yaml)"
	}
	return srcFloat(prof, src)
}

// readSet is this session's file knowledge, made on first use. Lazily, because
// a Session is built in half a dozen places (NewPrimary, newChild, compaction,
// tests) and a nil map here is a panic in the middle of a write.
func (s *Session) readSet() *readSet {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.rs == nil {
		s.rs = &readSet{m: map[string]seenFile{}}
	}
	return s.rs
}

// noteCmd remembers the command this session is about to run. run_command and
// the verifier's check_cmd write arbitrarily and cannot be leased — we cannot
// know what a command will write — so they are ACCOUNTED FOR instead: when a
// file turns out to have moved at or after this moment, the guard names the
// command rather than blaming a stranger for the session's own `sed -i`.
func (s *Session) noteCmd(line string) {
	s.mu.Lock()
	s.lastCmd.line, s.lastCmd.at = line, time.Now()
	s.mu.Unlock()
}

func (s *Session) cmdSince(mod time.Time) (string, time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lastCmd.line == "" || mod.Before(s.lastCmd.at) {
		return "", time.Time{}
	}
	return s.lastCmd.line, s.lastCmd.at
}

// noteRead / checkStale are the "read it before you change it" guard, for a
// local project or a remote one (where the fingerprint comes back over ssh).
// `shown` is what the model was given and `whole` says those bytes were the
// ENTIRE file: only then is a fingerprint recorded, so a line range or a
// truncated head+tail can never be mistaken for knowledge of the whole file.
func (s *Session) noteRead(ctx context.Context, p, shown string, whole bool) {
	rem := s.remote()
	if rem == nil {
		s.noteLocalRead(s.jail(), p, shown, whole)
		return
	}
	rel, err := rem.relPath(p)
	if err != nil {
		return
	}
	mt, size, sum, ok := rem.statSum(ctx, rel)
	if !ok {
		return
	}
	sf := seenFile{mtime: time.Unix(mt, 0), size: size, at: time.Now()}
	if whole {
		sf.sum = sumBytes([]byte(shown))
		if sum != "" && sum != sf.sum {
			// Record the MEMBER's hash, not ours. Remote.run merges stdout and stderr
			// (a condition this codebase already names: "something on %s writes to
			// stdout for non-interactive ssh (a shell rc file)"), so on a chatty
			// member every whole read disagrees with the member's own hash — and
			// poisoning the record there refused every later edit of every file for
			// ever. The member's hash is the one its own compare-and-swap will check,
			// so it is the one worth keeping; the disagreement is remembered beside it
			// and costs only the whole-file `write`.
			sf.sum, sf.noisy = sum, true
		}
	}
	s.readSet().note(rem.Label()+"/"+rel, sf)
}

// tornRead is the zero time, which no file's mtime can equal. A LOCAL record
// carrying it can never satisfy the cheap mtime+size comparison, so the guard is
// forced to compare the hash of what the model was actually shown — and locally
// that disagreement really is staleness: a rival landed inside the read→stat
// window, the model is holding old bytes, and a re-read fixes it.
var tornRead time.Time

// noteLocalRead records a read against an explicit jail. It exists for the two
// callers that read a local file while the session itself may be pinned to a
// member: an @file mention and a forked child's inherited context.
func (s *Session) noteLocalRead(j *Jail, p, shown string, whole bool) {
	abs, err := j.Resolve(p)
	if err != nil {
		return
	}
	info, err := os.Stat(abs)
	if err != nil {
		return
	}
	sf := seenFile{mtime: info.ModTime(), size: info.Size(), at: time.Now()}
	if whole {
		sf.sum = sumBytes([]byte(shown))
		// The stat happens AFTER the read, so a writer that landed in between gets
		// its own mtime and size recorded against bytes the model never saw — and
		// the cheap mtime+size comparison then vouches for them, so the next
		// whole-file `write` erases that writer silently. Which is the one outcome
		// the guarantee forbids.
		//
		// Comparing the file's HASH and not merely its length is what catches the
		// rival whose write happened to be the same size — a one-character
		// substitution, a formatter, two versions of the same line. One sha256 over
		// a file that is at most maxReadBytes long and still in the page cache, on a
		// path that is hashing those same bytes in memory anyway. The remote leg has
		// always compared hashes here; this is the local leg catching up.
		if sf.sum != sumFile(abs) {
			sf.mtime = tornRead
		}
	}
	s.readSet().note(abs, sf)
}

// seenSum is the hash of the bytes this session was SHOWN for a path, or "" when
// it has no whole-file record of it.
//
// It exists for the member's compare-and-swap, which must be told what the MODEL
// saw. A hash statted fresh just before the write is the RIVAL's hash whenever a
// rival wrote in the meantime, so handing that to the far side asks it to compare
// a value with itself — the swap passes and the rival's bytes go.
func (s *Session) seenSum(p string) string {
	if rem := s.remote(); rem != nil {
		if rel, err := rem.relPath(p); err == nil {
			if sf, ok := s.readSet().get(rem.Label() + "/" + rel); ok {
				return sf.sum
			}
		}
		return ""
	}
	if abs, err := s.jail().Resolve(p); err == nil {
		if sf, ok := s.readSet().get(abs); ok {
			return sf.sum
		}
	}
	return ""
}

// forgetRead drops a file's record. Used after a delegation's diff is merged
// into this tree: the caller has seen a DIFF, not the file, and a record there
// would license its next whole-file write to erase the merge.
func (s *Session) forgetRead(ctx context.Context, p string) {
	if rem := s.remote(); rem != nil {
		if rel, err := rem.relPath(p); err == nil {
			s.readSet().forget(rem.Label() + "/" + rel)
		}
		return
	}
	if abs, err := s.jail().Resolve(p); err == nil {
		s.readSet().forget(abs)
	}
}

// checkStale guards a targeted change (`edit`): the file must have been read and
// must not have moved since. checkStaleWhole is the same guard for a whole-file
// `write`, which additionally demands that the WHOLE file was read.
func (s *Session) checkStale(ctx context.Context, p string) string {
	return s.checkStaleFor(ctx, p, false)
}

func (s *Session) checkStaleWhole(ctx context.Context, p string) string {
	return s.checkStaleFor(ctx, p, true)
}

func (s *Session) checkStaleFor(ctx context.Context, p string, whole bool) string {
	rem := s.remote()
	if rem == nil {
		abs, err := s.jail().Resolve(p)
		if err != nil {
			return err.Error()
		}
		return s.checkStaleLocal(p, abs, whole)
	}
	rel, err := rem.relPath(p)
	if err != nil {
		return err.Error()
	}
	mt, size, sum, exists := rem.statSum(ctx, rel)
	if !exists {
		return "" // a new file; the create race is caught by the write itself
	}
	seen, ok := s.readSet().get(rem.Label() + "/" + rel)
	switch {
	case !ok:
		return fmt.Sprintf("%s has not been read in this session — read_file it first, then retry the change", p)
	case seen.sum != "" && sum != "":
		// When BOTH hashes are in hand the hash DECIDES, and the cheap mtime+size
		// branch is not cheaper, it is wrong. `stat` on a member has one-second
		// granularity (Remote.stat says so), so a rival's same-size overwrite inside
		// that second matches mtime and size exactly — and statSum has already paid
		// for the hash that proves otherwise, in the same round trip.
		if seen.sum != sum {
			return fmt.Sprintf("%s was modified on %s since it was last read — read it again before changing it", p, rem.Where())
		}
		// Identical bytes under a new timestamp: a touch, or a formatter that
		// changed nothing. Re-note so the next call compares against what is there.
		s.readSet().note(rem.Label()+"/"+rel, seenFile{mtime: time.Unix(mt, 0), size: size, sum: sum, noisy: seen.noisy, at: seen.at})
	case seen.mtime.Equal(time.Unix(mt, 0)) && seen.size == size:
	default:
		return fmt.Sprintf("%s was modified on %s since it was last read — read it again before changing it", p, rem.Where())
	}
	switch {
	case seen.noisy:
		return noisyReadMsg(p, rem)
	case whole && seen.sum == "":
		return partialReadMsg(p, size)
	}
	return ""
}

// noisyReadMsg refuses a change whose source bytes are not the file's, and says
// why WITHOUT blaming the file.
//
// It covers `edit` as well as `write`, which is the honest scope: Remote.run
// merges stdout and stderr, so on a member whose non-interactive shell prints a
// banner the bytes `cat` hands back are banner+content, and both writers build
// from them. `write` would store the banner; `edit` would store it too, because
// fuzzyReplace rewrites the whole string it was given. Before the guard existed
// that is exactly what happened, silently.
//
// The cause named here is the one this repository already names for the same
// symptom on the patch path, and it is a member's shell configuration: something
// the operator can fix and the model cannot. A staleness message instead put the
// model in a loop — re-reading reproduces the same banner, so the refusal never
// cleared.
func noisyReadMsg(name string, rem *Remote) string {
	return fmt.Sprintf("%s read back with bytes %s says are not in the file — something there writes to stdout for non-interactive ssh (a shell rc file), so a change would store that output in the file; guard it with [ -t 1 ], then read it again",
		name, rem.Where())
}

// checkStaleLocal is the local half, in order: no file is a new file, no record
// is "read it first", equal mtime AND size is ok, a matching hash under a
// different mtime is ok (and re-noted), anything else is stale.
func (s *Session) checkStaleLocal(name, abs string, whole bool) string {
	info, err := os.Stat(abs)
	if err != nil {
		return "" // new file: §create race is caught under the lease by the write
	}
	seen, ok := s.readSet().get(abs)
	if !ok {
		return fmt.Sprintf("%s has not been read in this session — read_file it first, then retry the change", name)
	}
	switch {
	case info.ModTime().Equal(seen.mtime) && info.Size() == seen.size:
	case seen.sum != "" && seen.sum == sumFile(abs):
		s.readSet().note(abs, seenFile{mtime: info.ModTime(), size: info.Size(), sum: seen.sum, at: seen.at})
	default:
		return s.staleMsg(name, seen, info)
	}
	if whole && seen.sum == "" {
		return partialReadMsg(name, info.Size())
	}
	return ""
}

// partialReadMsg refuses a whole-file overwrite built on a partial read. A
// `write` replaces every byte, so a model that saw lines 10-40 (or a truncated
// head+tail) would silently drop everything it never looked at — the same loss
// the lease prevents between writers, here between the model and the file.
//
// A file LARGER than a whole read can carry gets a different sentence, because
// it is a different refusal. readFile truncates above maxReadBytes, so no read
// of such a file ever records a fingerprint: telling the model to "read the
// whole file" there names a remedy the tool cannot perform, and it loops read →
// write → refused for ever. Regenerating a 300 KB lockfile or a large JSON is
// then impossible and the message does not say so. `edit` is the honest route,
// and naming it is what the >8 MiB branch of staleMsg already does.
func partialReadMsg(name string, size int64) string {
	if size > maxReadBytes {
		return fmt.Sprintf("%s is %s, more than read_file returns in one piece, so no read of it can license a whole-file write — use edit to change the part you have read", name, byteCount(int(size)))
	}
	return fmt.Sprintf("%s was only read in part — read the whole file before overwriting it with write, or use edit to change just the part you have", name)
}

func (s *Session) staleMsg(name string, seen seenFile, info os.FileInfo) string {
	if info.Size() > maxFingerprintBytes {
		return fmt.Sprintf("%s is %d MB, too large to fingerprint, so the guard can only compare its size and timestamp — they changed; read it again before changing it",
			name, info.Size()>>20)
	}
	if line, at := s.cmdSince(info.ModTime()); line != "" {
		return fmt.Sprintf("%s was modified since it was last read — your run_command `%s` at %s is the likely cause; read it again before changing it",
			name, line, at.Format("15:04:05"))
	}
	// One line, like every other guard message in this file. The two-line form
	// with a hanging indent was the spec document's own page wrapping, and a tool
	// result is not a page: it goes into the model's prompt, the transcript and
	// the trace exactly as written.
	return fmt.Sprintf("%s was modified since it was last read (you read it at %s, it was written at %s) — read it again before changing it",
		name, seen.at.Format("15:04:05"), info.ModTime().Format("15:04:05"))
}

func (s *Session) jail() *Jail {
	if s.jl != nil {
		return s.jl
	}
	return s.memberOf().jail(s.orch)
}

// runTool and runCheck are the only two paths from a tool, the verifier or a
// workflow step to the machine a session works on. The member's sandbox decides
// FIRST — Remote.run consults no policy of its own, and until members existed
// the remote branch of run_command called it with no CheckCommand at all, so the
// allowlist and the GPU policy were not enforced for the model's own commands on
// a remote project. A member must not be another way around them: nothing else
// calls Remote.run with a model's or an author's words.
//
// They are two methods, not one, because the two contracts differ and neither
// may drift: the model's tool keeps runCommand's live output, Ctrl-C handling
// and "error: …" refusals, while a verifier or a workflow step keeps execCheck's
// ("sandbox: …", -1), which the runner already looks for.
func (s *Session) runTool(ctx context.Context, cmd string, timeout time.Duration, live io.Writer) string {
	// Before it runs, not after: a file this command writes gets an mtime at or
	// after this moment, and the stale message has to be able to name it.
	s.noteCmd(cmd)
	if rem := s.remote(); rem != nil {
		if err := s.checkCmd(cmd); err != nil {
			return "error: " + err.Error()
		}
		out, exit := rem.run(ctx, cmd, timeout, nil, live)
		return remoteCmdResult(out, exit, timeout)
	}
	return runCommand(ctx, s.jail(), cmd, timeout, live)
}

func (s *Session) runCheck(ctx context.Context, cmd string, timeout time.Duration, live io.Writer) (string, int) {
	s.noteCmd(cmd) // a check_cmd writes too: `go build -o bin/x`, `gofmt -w`, a codegen step
	if rem := s.remote(); rem != nil {
		if err := s.checkCmd(cmd); err != nil {
			return "sandbox: " + err.Error(), -1
		}
		return rem.run(ctx, cmd, timeout, nil, live)
	}
	return execCheck(ctx, s.jail(), cmd, timeout, live)
}

// checkCmd asks this session's sandbox about a command it is about to run, with
// the semantics of the machine that will run it: a member's leg crosses a shell
// over there, and one-argv semantics would let a second command past the
// allowlist. The verifier asks the same question before it spends an attempt.
func (s *Session) checkCmd(cmd string) error {
	if s.remote() != nil {
		return s.jail().CheckRemote(cmd)
	}
	return s.jail().CheckCommand(cmd)
}

// SetAgent switches the primary agent. The system prompt is rebuilt (a
// one-time prefix-cache miss) and a note tells the model its mode changed.
func (s *Session) SetAgent(name string) error {
	ag := s.orch.agents[name]
	if ag == nil || !ag.isPrimary() {
		return fmt.Errorf("no primary agent %q", name)
	}
	prev := s.agent.Name
	s.agent = ag
	switch {
	case len(ag.Models) > 0:
		s.models = append([]string(nil), ag.Models...)
		s.useModel(0)
	case ag.Model != "":
		if err := s.SetModel(ag.Model); err != nil {
			return err
		}
	}
	s.RefreshSystem()
	if len(s.Msgs) > 1 && prev != name {
		s.Msgs = append(s.Msgs, Message{Role: "user", Content: fmt.Sprintf("[The user switched the agent from %s to %s. Follow the %s instructions in the system prompt from now on.]", prev, name, name)},
			Message{Role: "assistant", Content: "Understood.", Agent: name})
	}
	return nil
}

// RefreshSystem rebuilds the system prompt (after agent/model/transport change).
func (s *Session) RefreshSystem() {
	if len(s.Msgs) == 0 {
		s.Msgs = []Message{{Role: "system"}}
	}
	s.schemas, s.toolDefs = nil, nil
	s.Msgs[0].Content = s.systemPrompt()
}

// tools returns the session's tool definitions and native schemas, computed
// once: tool list and descriptions are part of the cached prefix, so they
// must not drift between calls (RefreshSystem resets them deliberately).
func (s *Session) tools() ([]*ToolDef, []ToolSchema) {
	if s.toolDefs != nil {
		return s.toolDefs, s.schemas
	}
	s.toolDefs = toolsFor(s)
	if s.toolDefs == nil {
		s.toolDefs = []*ToolDef{}
	}
	for _, t := range s.toolDefs {
		sc := t.schema()
		switch t.Name {
		case "task":
			sc.Function.Description = taskDescription(s)
		case "delegate":
			sc.Function.Description = delegateDescription(s)
		}
		s.schemas = append(s.schemas, sc)
	}
	return s.toolDefs, s.schemas
}

// activeTier is the tier every tier-declaring role was remapped to ("" = each
// role runs the tier it declares).
func (o *Orchestrator) activeTier() string {
	if o.roles == nil {
		return ""
	}
	return o.roles.Tier
}

// tier is the tier that produced this session's chain, for the trace and /model.
// A role with a chain of its own has no tier whatever the run selected, or a
// trace grouped by tier would credit its tokens to a chain it never ran.
func (s *Session) tier() string {
	if s.agent.Tier == "" {
		return ""
	}
	return firstNonEmpty(s.orch.activeTier(), s.agent.Tier)
}

// budget is the context budget for this session: the role's context limit,
// else LCA_CTX_TOKENS, else 75% of the model's window.
func (s *Session) budget() int {
	if s.agent.Context > 0 {
		// The role's number, but never above what the deployment reports. The
		// precedence is server max_model_len > role context: > profile, and a role
		// written above the running window does not raise the window — it builds
		// prompts the server refuses outright. doctor warns about the
		// disagreement; this is what keeps the run honest until the file is fixed.
		if n := s.client.ServerCtxLen(); n > 0 && n < s.agent.Context {
			return n * 3 / 4
		}
		return s.agent.Context * 3 / 4
	}
	return ctxBudget(s.orch.cfg.CtxTokens, s.client.CtxLen())
}

// budgetKnown reports whether budget() came from a real window — a role's
// context:, LCA_CTX_TOKENS, or what the deployment reports — rather than
// ctxBudget()'s placeholder. Only the screens that would draw a bar ask: a gauge
// whose denominator was invented is a lie in picture form, and a picture is
// believed before it is read.
func (s *Session) budgetKnown() bool {
	return s.agent.Context > 0 || s.orch.cfg.CtxTokens > 0 || s.client.CtxLen() > 0
}

// rules is the permission stack for this session, lowest precedence first
// (last match wins): defaults, built-in agent rules, the user's global
// permission config, the agent definition's own rules, session restrictions.
func (s *Session) rules() []Ruleset {
	var iso Ruleset
	if s.isolated {
		iso = isolationRules
	}
	return []Ruleset{defaultRules(), iso, s.agent.BaseRules, s.orch.userRules, s.agent.Rules, s.extra}
}

// isolationRules apply in a delegate's scratch worktree: editing and running
// sandboxed commands there is the job, and the verifier + the parent's apply
// step gate what reaches the real tree. Scheduling GPU work still asks.
var isolationRules = Ruleset{
	{"edit", "*", Allow},
	{"run", "*", Allow},
	{"run", "bsk *", Ask},
	// A worktree shares refs, stashes and config with the user's repository.
	{"run", "git push *", Ask},
	{"run", "git stash *", Ask},
	{"run", "git branch *", Ask},
	{"run", "git tag *", Ask},
	{"run", "git update-ref *", Ask},
	{"run", "git remote *", Ask},
	{"run", "git config *", Ask},
	{"run", "git worktree *", Ask},
	{"run", "git reflog *", Ask},
	{"run", "git gc *", Ask},
	{"run", "git -C *", Ask},
	{"run", "* -exec*", Ask},
	{"run", "* -execdir*", Ask},
	{"run", "* -ok *", Ask},
}

func (s *Session) event(kind string, fields map[string]any) {
	if s.parent != nil {
		if fields == nil {
			fields = map[string]any{}
		}
		fields["task_id"] = s.ID
		fields["agent"] = s.agent.Name
	}
	s.orch.rec.Event(kind, fields)
}

func (s *Session) saveTranscript() {
	if s.parent == nil {
		s.orch.rec.Transcript(s.Msgs)
	} else {
		s.orch.rec.ChildTranscript(s.ID, s.Msgs)
	}
}

func (s *Session) setTodos(t []Todo) {
	s.mu.Lock()
	s.todos = t
	s.mu.Unlock()
	s.view.Todos(t)
}

func (s *Session) Todos() []Todo {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Todo(nil), s.todos...)
}

// deliver queues a finished background subagent's result for the next step.
func (s *Session) deliver(result string) {
	s.mu.Lock()
	s.inbox = append(s.inbox, result)
	s.bgRunning--
	s.mu.Unlock()
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// drainInbox appends queued background results as one user message.
func (s *Session) drainInbox() bool {
	s.mu.Lock()
	items := s.inbox
	s.inbox = nil
	s.mu.Unlock()
	if len(items) == 0 {
		return false
	}
	s.Msgs = append(s.Msgs, Message{Role: "user", Content: "[Background subagent results]\n" + strings.Join(items, "\n")})
	return true
}

// SessionStats is what /stats reports. The number to watch is the share of tool
// calls that didn't parse: above a fraction of a percent it is the harness or
// the server's parser, not the model.
type SessionStats struct {
	Turns, ToolCalls, InvalidCalls, ToolErrors int
	PromptTokens, CachedTokens, OutputTokens   int
	Fallbacks, Compactions, VerifyRuns         int
}

// pendingInbox reports delivered background results not yet handed to the model.
func (s *Session) pendingInbox() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.inbox) > 0
}

func (s *Session) BackgroundRunning() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.bgRunning
}

func (s *Session) maxSteps() int {
	if s.agent.Steps > 0 {
		return s.agent.Steps
	}
	return s.orch.cfg.MaxSteps
}

func (s *Session) thinking() string {
	return firstNonEmpty(s.agent.Thinking, s.orch.cfg.Thinking, s.orch.fcThinking())
}

func (o *Orchestrator) fcThinking() string {
	if o.fc != nil {
		return o.fc.Thinking
	}
	return ""
}

const doneMarker = "TASK_DONE"

const maxStepsPrompt = `[MAXIMUM STEPS REACHED] Tools are disabled for this reply. Do not call any tool.
Respond with text only: state that the step limit was reached, summarize what was accomplished, and list the remaining work.`

// pendingCall is one tool invocation extracted from a reply, from either transport.
type pendingCall struct {
	id     string // native tool_call id ("" for text tags)
	name   string
	def    *ToolDef
	args   Args
	raw    string // arguments exactly as the model emitted them, for parse failures
	err    error
	native bool

	// outcome, for the trace
	invalid     bool // malformed call: unknown tool, bad JSON, args not matching the schema
	failed      bool
	errText     string
	resultBytes int
	ms          int64
}

// Run drives the agent loop for the transcript as it stands (the caller has
// appended the user message): call the model, execute the tools it asks for,
// feed results back, repeat until it answers without tools or hits the step
// budget. Returns nil on a normal finish, context.Canceled on interrupt.
func (s *Session) Run(ctx context.Context) error {
	s.LastReason = ""
	s.repairTranscript()
	continuing := false // the previous step was cut off by length; continue it
	partial := -1       // index of the assistant message being continued
	nudges := 0
	overflowRetried := false
	maxSteps := s.maxSteps()
	var stop atomic.Bool // a tool asked to end the turn (doom loop denied, interrupt)

	for step := 0; step < maxSteps && !stop.Load(); step++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		last := step == maxSteps-1
		if last {
			continuing = false // the final step is a plain summary request
		}
		// While continuing a cut-off reply the partial assistant message must
		// stay the last one sent, so nothing is appended or compacted meanwhile.
		if !continuing {
			s.drainInbox()
		}

		budget := s.budget()
		send, trimmed := trimForContext(s.Msgs, budget)
		if trimmed > 0 {
			s.view.Note(fmt.Sprintf("CONTEXT  trimmed %d old tool outputs (~%dk budget)", trimmed, budget/1000))
			s.event("context_trim", map[string]any{"collapsed": trimmed, "budget_tokens": budget})
		}
		// Still over budget after trimming: compact the conversation into a
		// summary rather than letting the server reject the request.
		if !continuing && budget > 0 && estimateTokens(send) > budget && len(s.Msgs) > 4 {
			if err := s.Compact(ctx, true); err != nil {
				s.view.Warn("auto-compact failed: " + err.Error())
			} else {
				send = s.Msgs
			}
		}

		if last {
			send = append(append([]Message(nil), send...), Message{Role: "user", Content: maxStepsPrompt})
		}

		req := ChatRequest{Messages: send, Temperature: s.agent.Temperature, TopP: s.agent.TopP,
			Thinking: s.thinking(), ContinueFinal: continuing}
		s.applyModelOpts(&req)
		if _, schemas := s.tools(); s.client.Native() && !last { // on the last step tools are disabled
			req.Tools = schemas
		}

		// A cancelable step context; on the interactive primary a scoped SIGINT
		// handler lets Ctrl-C abort a runaway generation mid-flight.
		stepCtx, cancel := context.WithCancel(ctx)
		var interrupted atomic.Bool
		stopSig := s.watchInterrupt(stepCtx, cancel, &interrupted)
		sv := s.view.Stream()
		sv.Start(s.client.Model())
		turnStart := time.Now()
		res, fallbacks, err := s.chat(stepCtx, req, sv.Sink())
		for _, fb := range fallbacks {
			s.event("gw_fallback", map[string]any{"from": fb.From, "to": fb.To, "reason": fb.Reason, "wait_ms": fb.WaitMs})
		}
		stopSig()
		cancel()
		reasonLines := sv.End()
		s.view.Perf(res.Usage)
		s.lastUsage = res.Usage
		s.stats.Turns++
		s.stats.PromptTokens += res.Usage.PromptTokens
		s.stats.CachedTokens += res.Usage.CachedTokens
		s.stats.OutputTokens += res.Usage.CompletionTokens
		s.stats.Fallbacks += len(fallbacks)
		if res.Usage.PromptTokens > 0 {
			s.event("usage", map[string]any{"model": s.client.Ref(), "prompt": res.Usage.PromptTokens, "cached": res.Usage.CachedTokens,
				"completion": res.Usage.CompletionTokens, "tok_s": res.Usage.TokPerSec()})
		}
		if len(reasonLines) > 0 {
			if s.LastReason != "" {
				s.LastReason += "\n"
			}
			s.LastReason += strings.Join(reasonLines, "\n")
		}

		// On a continuation, extend the same assistant message so a tool block
		// split by the length limit reassembles; otherwise start a new one. The
		// partial reply is kept on interrupt too, so the transcript alternates.
		if continuing && partial >= 0 && partial < len(s.Msgs) {
			lastMsg := &s.Msgs[partial]
			lastMsg.Content += res.Content
			lastMsg.Reasoning += res.Reasoning
			lastMsg.ToolCalls = append(lastMsg.ToolCalls, res.ToolCalls...)
		} else if res.Content != "" || len(res.ToolCalls) > 0 || !interrupted.Load() {
			s.Msgs = append(s.Msgs, Message{Role: "assistant", Content: res.Content, Reasoning: res.Reasoning,
				ToolCalls: res.ToolCalls, Agent: s.agent.Name})
		}
		continuing = false

		if interrupted.Load() || ctx.Err() != nil {
			// Native tool calls without results would make the next request
			// invalid — drop them from the partial message.
			if n := len(s.Msgs); n > 0 && s.Msgs[n-1].Role == "assistant" {
				s.Msgs[n-1].ToolCalls = nil
			}
			// Ctrl-C used to leave the abandoned work standing in the transcript: the
			// model's own "I'll continue reading the files" and its todo list were the
			// last thing in the conversation, so the operator's NEXT message — even
			// "hello" — made it resume, on whatever endpoint was current by then. The
			// stop has to be written down, not only printed.
			s.noteInterrupted()
			s.view.Note("interrupted")
			s.traceTurn(step, res, fallbacks, turnStart, nil, context.Canceled)
			s.event("interrupt", map[string]any{"partial_bytes": len(res.Content)})
			s.saveTranscript()
			return context.Canceled
		}
		if err != nil {
			if n := len(s.Msgs); n > 0 && s.Msgs[n-1].Role == "assistant" && s.Msgs[n-1].Content == "" && len(s.Msgs[n-1].ToolCalls) == 0 {
				s.Msgs = s.Msgs[:n-1] // drop the empty reply of a failed request
			}
			if isContextOverflow(err) && !overflowRetried && len(s.Msgs) > 3 {
				overflowRetried = true
				// The refusal usually names the real window. Learning it here is what
				// turns "compact and hope" into "compact to fit": the budget for the
				// retry is computed from the server's own number, and it is remembered
				// for the next session so this costs one refusal per deployment, ever.
				if n := statedWindow(err); n > 0 && n != s.client.CtxLen() {
					s.client.SetCtxLen(n)
					s.orch.providers.RememberWindow(s.client.Endpoint(), s.client.Model(), n)
					s.view.Note(fmt.Sprintf("%s serves a %s window — learned from its own refusal and remembered",
						s.client.Model(), kfmt(n)))
				}
				s.view.Warn("context window exceeded — compacting and retrying")
				s.traceTurn(step, res, fallbacks, turnStart, nil, err)
				if cerr := s.Compact(ctx, true); cerr == nil {
					step--
					continue
				}
			}
			s.traceTurn(step, res, fallbacks, turnStart, nil, err)
			msg := fmt.Sprintf("%s failed: %s", s.client.Model(), shortErr(err))
			if h := errorHint(err); h != "" {
				msg += "\n" + h
			}
			s.view.Error(msg)
			s.event("error", map[string]any{"err": err.Error(), "model": s.client.Ref()})
			s.saveTranscript()
			return err
		}

		full := s.Msgs[len(s.Msgs)-1].Content
		calls := s.extractCalls(s.Msgs[len(s.Msgs)-1])
		if last {
			// Tools were disabled for this reply: never execute what came back.
			if len(calls) > 0 {
				s.Msgs[len(s.Msgs)-1].ToolCalls = nil
				calls = nil
			}
			s.traceTurn(step, res, fallbacks, turnStart, nil, nil)
			s.view.Warn(fmt.Sprintf("STOPPED — hit %d-step cap", maxSteps))
			s.event("step_cap", map[string]any{"steps": maxSteps})
			s.saveTranscript()
			return nil
		}

		if len(calls) == 0 {
			malformed := s.malformed
			s.stats.InvalidCalls += malformed
			s.traceTurn(step, res, fallbacks, turnStart, nil, nil)
			// Text protocol: a tag that didn't parse is an invalid call, like bad
			// JSON on the native transport — tell the model instead of silently
			// ending the turn.
			if malformed > 0 && nudges < 3 {
				nudges++
				s.view.Note(fmt.Sprintf("%d tool tag(s) didn't parse — asking the model to re-emit", malformed))
				s.event("malformed_tags", map[string]any{"count": malformed})
				s.Msgs = append(s.Msgs, Message{Role: "user", Content: fmt.Sprintf("%s %d tool tag(s) in your reply could not be parsed, so NOTHING was executed. Typical causes: a missing closing tag line (e.g. </write>), a tag not alone on its line, or an <edit> without both <search> and <replace> sections. Re-emit the call(s) exactly per the protocol.", malformedPrefix, malformed)})
				s.saveTranscript()
				continue
			}
			if res.Finish == "length" {
				s.view.Note("response truncated — continuing…")
				s.event("auto_continue", map[string]any{"finish": res.Finish})
				if s.client.provider.Local && !s.client.Native() {
					continuing = true
					partial = len(s.Msgs) - 1
				} else {
					s.Msgs = append(s.Msgs, Message{Role: "user", Content: "Your reply was cut off by the output limit. Continue exactly where it stopped."})
				}
				continue
			}
			// Loop mode: keep working autonomously until the model signals it is
			// finished with TASK_DONE (or the step budget runs out).
			if s.Loop && !strings.Contains(full, doneMarker) {
				s.view.Note("loop — continuing…")
				s.event("loop_continue", nil)
				s.Msgs = append(s.Msgs, Message{Role: "user", Content: "Keep going — take the next action with a tool call. When the ENTIRE task is truly finished, reply with just " + doneMarker + " on its own line."})
				s.saveTranscript()
				continue
			}
			// The model described a change as a diff instead of an edit call, so
			// nothing was applied — ask it to redo it as a real tool call.
			if nudges < 2 && looksLikeStrayEdit(full) {
				nudges++
				s.view.Note("that was a diff, not an edit — asking for a tool call…")
				s.event("nudge_edit", nil)
				s.Msgs = append(s.Msgs, Message{Role: "user", Content: "That change was shown as a diff / code block, which does NOT modify any file. Redo it now as an edit or write tool call, then stop."})
				s.saveTranscript()
				continue
			}
			if nudges < 2 && looksStalled(full) {
				nudges++
				s.view.Note("continuing…")
				s.event("nudge_continue", nil)
				s.Msgs = append(s.Msgs, Message{Role: "user", Content: "Continue with the next step now — make the tool call for it. Do not stop until the task is done."})
				s.saveTranscript()
				continue
			}
			// A background result that landed while this reply was being written
			// must be read before the turn can end.
			if s.pendingInbox() {
				step--
				continue
			}
			// Background subagents still running: this agent isn't done until
			// they report back.
			if n := s.BackgroundRunning(); n > 0 {
				s.view.Note(fmt.Sprintf("waiting for %d background subagent(s)…", n))
				if !s.waitBackground(ctx) {
					s.noteInterrupted()
					s.saveTranscript()
					return context.Canceled
				}
				step-- // waiting isn't a model step
				continue
			}
			s.saveTranscript()
			return nil
		}

		results := s.execCalls(ctx, calls, &stop)
		s.stats.ToolCalls += len(calls)
		s.stats.InvalidCalls += s.malformed
		for i := range calls {
			if calls[i].invalid {
				s.stats.InvalidCalls++
			}
			if calls[i].failed {
				s.stats.ToolErrors++
			}
		}
		s.traceTurn(step, res, fallbacks, turnStart, calls, nil)
		s.appendResults(calls, results)
		if s.interruptedAtDoor.Swap(false) {
			s.noteInterrupted()
			s.view.Note("interrupted")
			s.saveTranscript()
			return context.Canceled
		}
		if ctx.Err() != nil {
			// Stopped while a tool was running — the commonest Ctrl-C of all, and the
			// one that left the abandoned plan standing.
			s.noteInterrupted()
			s.saveTranscript()
			return ctx.Err()
		}
		s.saveTranscript()
	}
	s.saveTranscript()
	return nil
}

// repairTranscript gives every native tool call a result. A session killed
// mid-tool (then resumed) would otherwise send dangling tool_calls, which
// chat-completions APIs reject. Missing results are inserted right after the
// results that are present.
func (s *Session) repairTranscript() {
	for i := 0; i < len(s.Msgs); i++ {
		m := s.Msgs[i]
		if m.Role != "assistant" || len(m.ToolCalls) == 0 {
			continue
		}
		have := map[string]bool{}
		j := i + 1
		for ; j < len(s.Msgs) && s.Msgs[j].Role == "tool"; j++ {
			have[s.Msgs[j].ToolCallID] = true
		}
		var missing []Message
		for _, tc := range m.ToolCalls {
			if !have[tc.ID] {
				missing = append(missing, Message{Role: "tool", ToolCallID: tc.ID, Tool: tc.Function.Name, Content: "error: [tool execution was interrupted]"})
			}
		}
		if len(missing) > 0 {
			tail := append([]Message(nil), s.Msgs[j:]...)
			s.Msgs = append(append(s.Msgs[:j], missing...), tail...)
		}
	}
}

// watchInterrupt installs a SIGINT → cancel handler for the interactive primary.
func (s *Session) watchInterrupt(ctx context.Context, cancel context.CancelFunc, flag *atomic.Bool) func() {
	if !s.TTY {
		return func() {}
	}
	sigch := make(chan os.Signal, 1)
	signal.Notify(sigch, os.Interrupt)
	done := make(chan struct{})
	go func() {
		select {
		case <-sigch:
			flag.Store(true)
			cancel()
		case <-ctx.Done():
		case <-done:
		}
	}()
	return func() {
		signal.Stop(sigch)
		close(done)
	}
}

func (s *Session) waitBackground(ctx context.Context) bool {
	stepCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var flag atomic.Bool
	stopSig := s.watchInterrupt(stepCtx, cancel, &flag)
	defer stopSig()
	for {
		s.mu.Lock()
		ready := len(s.inbox) > 0 || s.bgRunning == 0
		s.mu.Unlock()
		if ready {
			return true
		}
		select {
		case <-s.wake: // re-check: a stale token from an already-drained delivery finds nothing
		case <-stepCtx.Done():
			s.view.Note("stopped waiting — background subagents keep running (see /tasks)")
			return false
		}
	}
}

// extractCalls gets tool calls from an assistant message: native tool_calls if
// present, else text-protocol tags. Tags are honored even on a native
// transport — some open models fall back to writing them — so the work isn't lost.
func (s *Session) extractCalls(m Message) []pendingCall {
	var out []pendingCall
	s.malformed = 0
	if len(m.ToolCalls) > 0 {
		for _, tc := range m.ToolCalls {
			pc := pendingCall{id: tc.ID, name: tc.Function.Name, native: true, raw: tc.Function.Arguments}
			pc.def = resolveToolName(tc.Function.Name)
			pc.args, pc.err = decodeArgs(tc.Function.Arguments)
			out = append(out, pc)
		}
		return out
	}
	blocks := ParseBlocks(m.Content)
	// Text protocol: tool tags the model opened but the parser couldn't turn
	// into a call (unterminated, broken edit sections…) are invalid calls too.
	if opened := countToolTagOpens(m.Content); opened > len(blocks) {
		s.malformed = opened - len(blocks)
	}
	for _, b := range blocks {
		pc := pendingCall{name: b.Name, def: toolRegistry[b.Name], args: Args{}}
		for k, v := range b.Attr {
			pc.args[k] = v
		}
		switch {
		case b.Name == "edit":
			pc.args["old_string"], pc.args["new_string"] = b.Search, b.Replace
		case pc.def != nil && pc.def.Body != "":
			pc.args[pc.def.Body] = b.Body
		}
		out = append(out, pc)
	}
	return out
}

const malformedPrefix = "[protocol error]"

// noteInterrupted writes the stop into the transcript, once. Idempotent because
// two exits can both be reached on the way out of one turn.
func (s *Session) noteInterrupted() {
	if n := len(s.Msgs); n > 0 && s.Msgs[n-1].Role == "user" && s.Msgs[n-1].Content == interruptNote {
		return
	}
	s.Msgs = append(s.Msgs, Message{Role: "user", Content: interruptNote})
}

// interruptNote is what the transcript says where the turn stopped. It is
// addressed to the model, so it names every way "continue anyway" tends to
// happen: the plan, the todo list, and the tool call that was about to run.
const interruptNote = "[The user pressed Ctrl-C and stopped the turn above. That work is ABANDONED. " +
	"Do not resume it, do not carry on with its plan or its todo list, and do not make the tool calls it was " +
	"about to make. Answer the user's next message on its own terms; if they want the abandoned work continued, " +
	"they will ask for it.]"

// countToolTagOpens counts lines that open a tool tag (the parser's view of
// the text, reasoning stripped).
func countToolTagOpens(text string) int {
	n := 0
	for _, ln := range strings.Split(normalizeTags(reThinkBlock.ReplaceAllString(text, "")), "\n") {
		t := strings.TrimSpace(ln)
		if !strings.HasPrefix(t, "<") || strings.HasPrefix(t, "</") {
			continue
		}
		name := t[1:]
		if i := strings.IndexAny(name, " />"); i >= 0 {
			name = name[:i]
		}
		if blockNames[name] {
			n++
		}
	}
	return n
}

// execCalls runs a reply's tool calls. If every call is parallel-safe (reads,
// searches, subagents) they run concurrently; otherwise in order, so approvals
// and side effects stay sequential. Results keep the call order.
// rootGone reports the working directory having disappeared under a running
// session — the project deleted or moved, a mount lost — as one message for the
// model and the operator. Only local trees: a member's directory is probed by
// the member's own gate, which says which machine it was.
func (s *Session) rootGone() string {
	if s.remote() != nil {
		return ""
	}
	root := s.jail().Root
	if st, err := os.Stat(root); err == nil && st.IsDir() {
		return ""
	}
	return fmt.Sprintf("error: the working directory %s no longer exists — nothing can run here. "+
		"Start lca again from a directory that exists; this session's transcript is saved.", root)
}

func (s *Session) execCalls(ctx context.Context, calls []pendingCall, stop *atomic.Bool) []string {
	results := make([]string, len(calls))
	parallel := len(calls) > 1
	hasTask := false
	for _, c := range calls {
		if c.def == nil || !c.def.Parallel && c.def.Name != "task" {
			parallel = false
		}
		if c.def != nil && c.def.Name == "task" {
			hasTask = true
		}
	}
	// A vanished working directory is not a per-tool failure: without it nothing
	// can run, and a model told "no such file" five times will keep inventing new
	// ways to look. One honest error, and the turn ends.
	if msg := s.rootGone(); msg != "" {
		for i := range calls {
			results[i] = msg
			calls[i].failed, calls[i].errText = true, msg
		}
		s.view.Error(strings.TrimPrefix(msg, "error: "))
		stop.Store(true)
		return results
	}
	// Subagents run for a while: let Ctrl-C cancel them (and this turn).
	batchCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var interrupted atomic.Bool
	if hasTask {
		defer s.watchInterrupt(batchCtx, cancel, &interrupted)()
	}

	if parallel {
		var wg sync.WaitGroup
		for i := range calls {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				results[i] = s.timedCall(batchCtx, &calls[i], stop)
			}(i)
		}
		wg.Wait()
	} else {
		for i := range calls {
			if batchCtx.Err() != nil || stop.Load() {
				results[i] = "error: not executed (the turn was interrupted or stopped)"
				continue
			}
			results[i] = s.timedCall(batchCtx, &calls[i], stop)
		}
	}
	if interrupted.Load() {
		stop.Store(true)
	}
	return results
}

func (s *Session) timedCall(ctx context.Context, c *pendingCall, stop *atomic.Bool) string {
	start := time.Now()
	res := s.execCall(ctx, c, stop)
	c.ms = time.Since(start).Milliseconds()
	c.resultBytes = len(res)
	if strings.HasPrefix(res, "error:") || strings.HasPrefix(res, "user denied") {
		c.failed, c.errText = true, res
	}
	return res
}

var doomMu sync.Mutex

func (s *Session) execCall(ctx context.Context, c *pendingCall, stop *atomic.Bool) string {
	if c.def == nil {
		c.invalid = true
		names := make([]string, 0)
		defs, _ := s.tools()
		for _, t := range defs {
			names = append(names, t.Name)
		}
		return fmt.Sprintf("error: unknown tool %q. Available tools: %s", c.name, strings.Join(names, ", "))
	}
	if c.err != nil {
		c.invalid = true
		return fmt.Sprintf("error: the %s tool was called with invalid arguments: %v. Rewrite the call with valid JSON arguments", c.def.Name, c.err)
	}
	// A role's tools: list is a CAPABILITY and not only a prefix. extractCalls
	// resolves a name against the process-wide registry — natively through
	// resolveToolName, in the text transport straight out of toolRegistry — so a
	// model that names a tool it was never shown (guessed, carried over from a
	// resumed transcript, or suggested by injected ticket text) used to reach it
	// whenever its permission was allow-by-default. mcp_read is allow-by-default, so
	// "a role that names no MCP tool gets none of them" held for the prefix and not
	// for reach. This is the same predicate toolsFor uses for the prefix, enforced
	// where the call happens, and it closes the equivalent gap for builtins.
	if s.agent.ToolsSet && !contains(s.agent.Tools, c.def.Name) {
		return fmt.Sprintf("error: the %s tool is not available to the %s agent", c.def.Name, s.agent.Name)
	}
	if Disabled(permissionOf(c.def.Name), s.rules()...) {
		return fmt.Sprintf("error: the %s tool is not available to the %s agent", c.def.Name, s.agent.Name)
	}
	if err := c.def.validate(c.args); err != nil {
		c.invalid = true
		return "error: " + err.Error()
	}

	// Doom-loop guard (opencode: 3 identical calls in a row → ask).
	sig := c.def.Name + ":" + canonicalArgs(c.args)
	doomMu.Lock()
	s.recent = append(s.recent, sig)
	if len(s.recent) > 3 {
		s.recent = s.recent[len(s.recent)-3:]
	}
	looping := len(s.recent) == 3 && s.recent[0] == sig && s.recent[1] == sig
	doomMu.Unlock()
	if looping {
		tc := &ToolCtx{Ctx: ctx, S: s, Name: c.def.Name}
		if msg, ok := tc.Ask("doom_loop", c.def.Name, "REPEATED CALL "+c.def.Name+" (3× identical)", "  the agent is calling "+c.def.Name+" with the same arguments again"); !ok {
			stop.Store(true)
			s.event("doom_loop", map[string]any{"tool": c.def.Name})
			return msg + " — you called this tool 3 times with identical arguments; stop and reconsider the approach"
		}
	}

	s.view.ToolStart(c.def.Name, toolSummary(c.def.Name, c.args))
	res := c.def.Run(&ToolCtx{Ctx: ctx, S: s, CallID: c.id, Name: c.def.Name}, c.args)
	s.view.ToolDone(c.def.Name, c.args, res)
	// An MCP result is somebody's ticket. summarize() below would put 200 bytes of
	// it in the audit log, which outlives every transcript here. MCP tools audit
	// themselves instead, with argument KEYS and never argument values.
	if mcpTools[c.def.Name] != nil {
		return res
	}
	switch c.def.Name {
	case "edit", "write", "run_command", "task", "todowrite", "skill", "webfetch":
		// these audit themselves with richer fields
	default:
		s.event(c.def.Name, map[string]any{"args": toolSummary(c.def.Name, c.args), "result": summarize(res)})
	}
	return res
}

func canonicalArgs(a Args) string {
	b, _ := json.Marshal(map[string]any(a)) // map keys are sorted by encoding/json
	return string(b)
}

// appendResults adds tool results to the transcript in the shape each
// transport expects.
func (s *Session) appendResults(calls []pendingCall, results []string) {
	var text strings.Builder
	for i, c := range calls {
		if c.native {
			s.Msgs = append(s.Msgs, Message{Role: "tool", ToolCallID: c.id, Content: results[i], Tool: c.name, Path: c.args.Str("path")})
			continue
		}
		text.WriteString(toolResultText(c.name, c.args.Str("path"), results[i]))
	}
	if text.Len() > 0 {
		s.Msgs = append(s.Msgs, Message{Role: "user", Content: text.String()})
	}
}

// Ask resolves a permission for the running tool: allow → proceed; deny → an
// error for the model; ask → the interactive approval gate.
func (tc *ToolCtx) Ask(permission, pattern, header, preview string) (string, bool) {
	s := tc.S
	if permission == "read" || permission == "edit" {
		pattern = permPath(s.jail(), pattern)
	}
	act := Evaluate(permission, pattern, s.rules()...)
	// With a shell (unsafe mode) an allow rule like "git status *" would also
	// match "git status; rm -rf ~" — a chained command always asks.
	if act == Allow && permission == "run" && s.jail().Unsafe && reShellChain.MatchString(pattern) {
		act = Ask
	}
	switch act {
	case Allow:
		return "", true
	case Deny:
		s.event("permission_denied", map[string]any{"permission": permission, "pattern": pattern})
		return fmt.Sprintf("error: denied by a permission rule (%s: %q) for the %s agent — don't retry this call; choose another approach or ask the user", permission, pattern, s.agent.Name), false
	}
	if s.parent != nil {
		header = "[" + s.agent.Name + " " + s.ID + "] " + header
	}
	// A background subagent can't prompt: the REPL may own the terminal. Only
	// standing trust (/approve, -y, allow rules) lets it act.
	if s.background {
		if s.orch.ap.Trusts(permission) {
			s.event("auto_approve", map[string]any{"permission": permission, "pattern": pattern})
			return "", true
		}
		s.event("permission_denied", map[string]any{"permission": permission, "pattern": pattern, "background": true})
		return "error: this needs approval, and a background subagent cannot ask the user (" + header + "). Finish without it and report what still needs to be done, or ask to be rerun in the foreground", false
	}
	ok, auto := s.orch.ap.Confirm(permission, header, preview)
	if ok && auto {
		s.event("auto_approve", map[string]any{"permission": permission, "pattern": pattern})
	}
	if !ok {
		if s.orch.ap.TakeInterrupt() {
			// Ctrl-C at the door is "stop", not "no to this one": end the turn the way
			// any other interrupt does, with the abandonment written down.
			s.interruptedAtDoor.Store(true)
			return "error: [interrupted by the user at the approval prompt — nothing ran]", false
		}
		return "user denied this action (" + header + ")", false
	}
	return "", true
}

// permPath normalizes a path for permission matching, so "./a/b", "a//b" and
// "/abs/root/a/b" all match a rule written as "a/b": jail-relative, cleaned,
// slash-separated. Paths outside the jail (unsafe mode) stay absolute.
func permPath(j *Jail, p string) string {
	if p == "" {
		return "."
	}
	abs := p
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(j.Root, p)
	}
	abs = filepath.Clean(abs)
	if rel, err := filepath.Rel(j.Root, abs); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return filepath.ToSlash(rel)
	}
	return filepath.ToSlash(abs)
}

// toolSummary is the one-line argument shown next to a tool marker.
func toolSummary(name string, a Args) string {
	// An MCP call has no path argument, so it used to render as a bare marker with
	// nothing after it. run_command shows its command and webfetch its URL; a read of
	// somebody's ticket showed neither the key nor the query — and mcp_read is
	// allow-by-default, so no approval question ever displayed them either. That
	// gutter line is the only visibility a read has.
	if mcpTools[name] != nil {
		args, keys, err := mcpArgs(a)
		if err != nil {
			return ""
		}
		keys = mcpArgOrder(args)
		for _, k := range keys {
			if v, ok := args[k].(string); ok && strings.TrimSpace(v) != "" {
				return truncate(collapseWS(v), 80)
			}
		}
		return truncate(strings.Join(keys, " "), 80)
	}
	switch name {
	case "read_file":
		s := a.Str("path")
		if l := a.Str("lines"); l != "" {
			s += ":" + l
		} else if o := a.Int("offset"); o > 0 {
			s += fmt.Sprintf(":%d+", o)
		}
		return s
	case "grep":
		s := fmt.Sprintf("%q %s", a.Str("pattern"), a.Str("path"))
		if inc := a.Str("include"); inc != "" {
			s += " (" + inc + ")"
		}
		return s
	case "glob":
		return strings.TrimSpace(a.Str("pattern") + " " + a.Str("path"))
	case "run_command":
		return truncate(a.Str("command"), 120)
	case "task":
		return firstNonEmpty(a.Str("agent"), a.Str("subagent_type")) + ": " + a.Str("description")
	case "delegate":
		return a.Str("role") + ": " + truncate(firstLine(a.Str("task")), 80)
	case "skill":
		return a.Str("name")
	case "webfetch":
		return a.Str("url")
	case "todowrite":
		return ""
	}
	return a.Str("path")
}

var reShellChain = regexp.MustCompile("[;&|`\n]|\\$\\(|>|<")

var reThinkBlock = regexp.MustCompile(`(?is)<(?:[a-z0-9_]+:)?(?:think|thinking|reasoning)>.*?</(?:[a-z0-9_]+:)?(?:think|thinking|reasoning)>`)

// finalText is the answer part of an assistant message: inline reasoning
// blocks and tool tags removed.
func finalText(m Message) string {
	t := reThinkBlock.ReplaceAllString(m.Content, "")
	if blocks := ParseBlocks(t); len(blocks) > 0 {
		var keep []string
		inBlock := ""
		for _, ln := range strings.Split(normalizeTags(t), "\n") {
			tr := strings.TrimSpace(ln)
			if inBlock != "" {
				if tr == inBlock {
					inBlock = ""
				}
				continue
			}
			if mm := reOpen.FindStringSubmatch(tr); mm != nil && blockNames[mm[1]] {
				if mm[3] != "/" && !voidTools[mm[1]] {
					inBlock = "</" + mm[1] + ">"
				}
				continue
			}
			keep = append(keep, ln)
		}
		t = strings.Join(keep, "\n")
	}
	return strings.TrimSpace(t)
}

// ChildTranscriptDir is where subagent transcripts are written.
func childTranscriptPath(dir, session, id string) string {
	return filepath.Join(dir, "transcripts", "subagents", session+"-"+id+".json")
}
