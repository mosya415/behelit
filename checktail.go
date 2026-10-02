package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// The check that takes minutes, and the twenty thousand lines it prints.
//
// A stand run is a build, a deploy and then `check.sh`: five to fifteen
// minutes, and an output long enough that the one line that decides the ticket
// is somewhere in the middle of it. The last sixty lines — which is what the
// verifier used to send back — are the test harness's own epilogue, and on a
// stand that epilogue is "3 of 412 failed, see above". The model is then told
// that something is wrong and shown the one part of the output that does not
// say what.
//
// So the output is cut three ways instead of one:
//
//   - the FULL output of every attempt goes to a file beside the transcript, so
//     nothing is lost and a person can go and look. Its path is in the result
//     object. The MODEL can go and look only when that file is inside the jail,
//     which under the default LCA_DIR it is not — so the tail never tells it to;
//     verify.go's fullOutputNote asks the jail and names the path when the answer
//     is yes.
//   - what the MODEL is sent is selected rather than truncated: every line that
//     matches error / FAIL / panicked / ✗ with five lines around it, plus the
//     last forty. Those are the requirement's own numbers.
//   - what is omitted is MARKED, with the line numbers it stood at, because a
//     tail that silently skips eight thousand lines reads as a complete log and
//     a model counting lines in it is counting the wrong ones.
//
// The budget is in bytes and not in lines, because the point of all this is to
// get one line in front of a model whose context is finite: a log where half
// the lines match is still a log that has to fit, and the line that matters is
// then named in the omission marker rather than silently dropped.

const (
	// The requirement's own two numbers: five lines of context around each
	// matching line, and the last forty whatever else is kept.
	checkTailContext = 5
	checkTailLast    = 40
	// checkTailBytes is what the whole selection may cost. Three times the old
	// plain tail (tailBytes), because it now has to carry a region from the
	// middle as well as the end — and still a ceiling, because this text is
	// appended to a conversation that goes round the attempt loop again: an
	// unbounded tail is a context window spent on one failed attempt.
	checkTailBytes = 12_000
	// checkTailTrailerBytes is reserved out of the budget above for the one line
	// that says something did not fit. Reserved rather than charged, because
	// whether it is printed is only known after the last claim has been refused —
	// and a ceiling that is exceeded by its own apology is still exceeded.
	checkTailTrailerBytes = 96
	// checkTailLineBytes bounds ONE line. A minified bundle, a base64 blob or a
	// progress bar that never got its newline is a single line of megabytes, and
	// one of those inside the last forty would eat the whole budget and push the
	// error back out of the selection.
	checkTailLineBytes = 2_000
)

// checkTailMarks are the words that mean "this line is why". Matched
// case-insensitively: the requirement spells them `error`, `FAIL`, `panicked`
// and `✗`, and a stand's own log says Error:, ERROR, error — the same fact in
// three spellings, and a case-sensitive match would keep the wrong one.
//
// They are deliberately coarse. A line saying "0 errors" matches too, and that
// is the right trade: the cost of a false positive is five lines of a budget
// measured in thousands, and the cost of a false negative is the ticket.
var checkTailMarks = []string{"error", "fail", "panicked", "✗"}

// checkTailOmitted is the omission marker's one spelling, in one place, because
// its WIDTH is charged to the budget before it is written (tailSelection) and a
// marker whose cost was computed from a different format string than the one
// that prints it is a ceiling that drifts.
const checkTailOmitted = "[… lines %d-%d omitted of %d …]"

// checkTailOf is what the model is shown of a check's output.
//
// Short output is returned UNCHANGED — byte for byte, no markers, no
// selection. That is not an optimisation: `go test ./...` on a red package
// prints forty lines, all of them load-bearing, and an operator reading
// check_tail in the result object should see what the command printed and not
// an edited version of it. The threshold is the old tail's own, so every output
// that used to arrive whole still does.
//
// Above it the selection is made in PRIORITY order, and the order is the whole
// of the correctness here. The lines that MATCHED come first, bare; then the end
// of the log; then the context around each match, with the radius shrinking
// while the budget runs out. Serving the last forty first — which is what this
// did — let forty long lines of a JSON deploy record spend the entire budget
// before the loop ever reached the `FAIL cannot bind port 8080` twenty rows
// above them, and the requirement's one acceptance criterion is that that line
// arrives. The two numbers the requirement names, five lines of context and the
// last forty, are a selection and not a priority order.
func checkTailOf(out string) string {
	out = strings.TrimRight(out, "\n")
	if out == "" {
		return ""
	}
	lines := strings.Split(out, "\n")
	if len(lines) <= tailLines && len(out) <= tailBytes {
		return out
	}
	sel := newTailSelection(lines)

	// 1. The matching lines themselves, from the FIRST forwards. In a build log
	// the first error is the cause and the rest are its consequences — a failed
	// compile followed by two hundred "unresolved symbol" lines — so when the
	// budget runs out it is the consequences that are dropped.
	marks := make([]int, 0, 16)
	for i, l := range lines {
		if failureAt(l) >= 0 {
			marks = append(marks, i)
		}
	}
	dropped := 0
	for _, m := range marks {
		if !sel.add(m) {
			dropped++
		}
	}

	// 2. The end of the log: the harness's own verdict, "3 of 412 failed, see
	// above". Walked backwards and SKIPPING what it cannot afford rather than
	// stopping at it, because one 1.5 KB line in the middle of the epilogue used
	// to end the walk and lose the thirty-nine ordinary lines behind it.
	for i := len(lines) - 1; i >= 0 && i >= len(lines)-checkTailLast; i-- {
		sel.add(i)
	}

	// 3. The context, five lines either side, per match and in file order. The
	// radius shrinks rather than the context vanishing: three lines of context
	// around the failure is worth more than none, and a budget that has been
	// spent on two hundred matching lines has little left.
	for _, m := range marks {
		if !sel.keep[m] {
			continue // the line itself did not fit; its context cannot help
		}
		for _, r := range []int{checkTailContext, 2, 1} {
			if sel.addRange(m-r, m+r) {
				break
			}
		}
	}

	return sel.render(dropped)
}

// tailSelection is the budget. It exists because checkTailBytes was documented
// as a ceiling and was not one: the omission markers were emitted after the
// accounting and never charged to it, which on a realistic 20 000-line harness
// log came to 15 160 bytes against a stated 12 000, and on a log of short lines
// — a build log with blank lines between failures — to 43 960, because an
// eleven-line region of blank lines costs eleven bytes while the marker
// announcing the gap in front of it costs thirty-eight. That text is appended to
// the conversation once per failed attempt, so an unbudgeted 3.7x is a context
// window spent on arithmetic nobody did.
//
// So every byte the render will emit is charged as it is claimed: the line as it
// will be CLIPPED, and one marker per gap. The marker is charged at its
// worst-case width (every number as wide as the line count), which makes the
// ceiling an over-estimate by a few bytes a gap rather than an under-estimate by
// a factor of three.
type tailSelection struct {
	lines  []string
	keep   []bool
	gaps   int // maximal runs of dropped lines; one marker is emitted per run
	marker int // bytes one marker costs
	spent  int
	budget int
}

func newTailSelection(lines []string) *tailSelection {
	n := len(lines)
	marker := len(fmt.Sprintf(checkTailOmitted+"\n", n, n, n))
	s := &tailSelection{
		lines: lines, keep: make([]bool, n), marker: marker,
		// The trailer is reserved rather than charged, because whether it is
		// printed is only known once the last claim has been refused.
		budget: checkTailBytes - checkTailTrailerBytes,
	}
	// Nothing is kept yet, so the whole output is one gap and one marker.
	s.gaps, s.spent = 1, marker
	return s
}

// gapDelta is how the number of markers changes when line i starts being kept:
// a kept line splits the gap it sat in (+1), closes a gap of exactly one line
// (-1), or shortens one from either end (0). The ends of the output count as
// kept neighbours — there is no gap beyond them to mark.
func (s *tailSelection) gapDelta(i int) int {
	left := i == 0 || s.keep[i-1]
	right := i == len(s.keep)-1 || s.keep[i+1]
	switch {
	case left && right:
		return -1
	case !left && !right:
		return 1
	}
	return 0
}

func (s *tailSelection) cost(i int) int { return len(s.render1(i)) + 1 }

// add keeps one line if it fits. It returns whether the line is kept, which for
// a line already kept is true.
func (s *tailSelection) add(i int) bool {
	if i < 0 || i >= len(s.keep) {
		return false
	}
	if s.keep[i] {
		return true
	}
	d := s.gapDelta(i)
	c := s.cost(i) + d*s.marker
	if s.spent+c > s.budget {
		return false
	}
	s.keep[i], s.gaps, s.spent = true, s.gaps+d, s.spent+c
	return true
}

// addRange is add for a group that is only worth having whole: a context window
// that fits on one side of its failure and not the other is a window that says
// the failure came out of nowhere. All of it or none of it, and the caller then
// asks for a smaller radius.
func (s *tailSelection) addRange(from, to int) bool {
	if from < 0 {
		from = 0
	}
	if to > len(s.keep)-1 {
		to = len(s.keep) - 1
	}
	spent, gaps := s.spent, s.gaps
	var set []int
	for i := from; i <= to; i++ {
		if s.keep[i] {
			continue
		}
		d := s.gapDelta(i)
		c := s.cost(i) + d*s.marker
		if s.spent+c > s.budget {
			for _, j := range set {
				s.keep[j] = false
			}
			s.spent, s.gaps = spent, gaps
			return false
		}
		s.keep[i], s.gaps, s.spent = true, s.gaps+d, s.spent+c
		set = append(set, i)
	}
	return true
}

// render1 is line i as the output will spell it: whole, or clipped — around the
// failure when it is one, so a check whose progress output never got a newline
// cannot hide its error in the part that was cut.
func (s *tailSelection) render1(i int) string {
	l := s.lines[i]
	if len(l) <= checkTailLineBytes {
		return l
	}
	if at := failureAt(l); at >= 0 {
		return clipLineAround(l, at)
	}
	return clipLine(l)
}

func (s *tailSelection) render(dropped int) string {
	var b strings.Builder
	b.Grow(s.spent)
	gap := 0
	for i := range s.lines {
		if !s.keep[i] {
			gap++
			continue
		}
		if gap > 0 {
			// 1-based and inclusive, so the numbers are the ones `sed -n` and
			// `grep -n` print for the full output file: the marker is an index into
			// the file, which is the only reason to print numbers at all.
			fmt.Fprintf(&b, checkTailOmitted+"\n", i-gap+1, i, len(s.lines))
			gap = 0
		}
		b.WriteString(s.render1(i))
		b.WriteByte('\n')
	}
	if gap > 0 {
		fmt.Fprintf(&b, checkTailOmitted+"\n", len(s.lines)-gap+1, len(s.lines), len(s.lines))
	}
	out := strings.TrimRight(b.String(), "\n")
	if dropped > 0 {
		// Said out loud, because the model is about to be asked to fix the cause:
		// "there are more of these" is the difference between it fixing one failure
		// and it believing it has seen them all.
		//
		// It does NOT tell the model where to look. The full output is a file under
		// $LCA_DIR, which defaults outside the worktree the sandbox confines the
		// model to, so `read_file` and `grep` on it are refused — and a remedy that
		// does not exist wastes the attempt it was meant to save. verify.go names
		// the path in the message when the model really can read it.
		out += fmt.Sprintf("\n[… %s matching the same words did not fit and were left out …]",
			plural(dropped, "line", "lines"))
	}
	return out
}

// marksAFailure is the per-line test, and failureAt is the same question with
// the answer's position, which is what lets a megabyte-long line be clipped
// around its error instead of through it.
//
// Neither lowercases the line. ToLower on every line of a twenty-thousand-line
// output allocated twenty thousand strings to ask four questions, and on a
// 2 MB single line it allocated 2 MB; indexFold asks the same question in place.
func marksAFailure(line string) bool { return failureAt(line) >= 0 }

func failureAt(line string) int {
	best := -1
	for _, m := range checkTailMarks {
		i := -1
		if m == "✗" {
			i = strings.Index(line, m)
		} else {
			i = indexFold(line, m)
		}
		if i >= 0 && (best < 0 || i < best) {
			best = i
		}
	}
	return best
}

// indexFold is strings.Index for an ALL-LOWERCASE ASCII needle, matched without
// regard to the haystack's case. b|0x20 maps a letter and its capital to the
// same byte and maps no other byte onto a lowercase letter, so there are no
// false positives and no copy.
func indexFold(s, lower string) int {
	n := len(lower)
	if n == 0 || len(s) < n {
		return -1
	}
	c := lower[0]
	for i := 0; i+n <= len(s); i++ {
		if s[i]|0x20 != c {
			continue
		}
		j := 1
		for ; j < n; j++ {
			if s[i+j]|0x20 != lower[j] {
				break
			}
		}
		if j == n {
			return i
		}
	}
	return -1
}

// clipLine cuts the middle out of one absurdly long line, keeping both ends:
// the start of a line is what names it and the end is where the assertion
// message usually is.
func clipLine(l string) string {
	if len(l) <= checkTailLineBytes {
		return l
	}
	head, tail := checkTailLineBytes*2/3, checkTailLineBytes/3
	return trimToRunes(l[:head]) + fmt.Sprintf("…[%d bytes cut]…", len(l)-head-tail) + trimToRunesLeft(l[len(l)-tail:])
}

// clipLineAround is clipLine for a line that MATCHED: the window is placed over
// the match instead of over the two ends, because a check whose progress output
// uses only \r prints its whole run as one line, and clipping that through the
// middle is how the error it was selected for gets cut out of the selection that
// selected it. A quarter of the window sits before the match, so there is some
// of what led up to it, and the rest after, where the message is.
func clipLineAround(l string, at int) string {
	if len(l) <= checkTailLineBytes {
		return l
	}
	start := at - checkTailLineBytes/4
	if start < 0 {
		start = 0
	}
	end := start + checkTailLineBytes
	if end > len(l) {
		end, start = len(l), len(l)-checkTailLineBytes
	}
	mid := trimToRunes(trimToRunesLeft(l[start:end]))
	var b strings.Builder
	if start > 0 {
		fmt.Fprintf(&b, "…[%d bytes cut]…", start)
	}
	b.WriteString(mid)
	if end < len(l) {
		fmt.Fprintf(&b, "…[%d bytes cut]…", len(l)-end)
	}
	return b.String()
}

// trimToRunes drops a trailing partial rune and trimToRunesLeft a leading one,
// so a Cyrillic message or a UTF-8 path is not left half-encoded in a field a
// JSON encoder is about to be handed. At most three bytes either way: that is
// the longest a truncated rune can be. They are independent, because a window
// cut out of the middle of a line is unaligned at both ends.
func trimToRunes(s string) string {
	for i := 0; i < 3 && len(s) > 0; i++ {
		if r, size := utf8.DecodeLastRuneInString(s); r != utf8.RuneError || size > 1 {
			break // a real rune, or a real U+FFFD somebody printed
		}
		s = s[:len(s)-1]
	}
	return s
}

func trimToRunesLeft(s string) string {
	for i := 0; i < 3 && len(s) > 0 && !utf8.RuneStart(s[0]); i++ {
		s = s[1:]
	}
	return s
}

// ── the full output of every attempt ────────────────────────────────────────

// CheckLog writes one check run's whole output beside the transcript and
// returns the path, or "" and the reason it could not.
//
// It lives in this file and not in recorder.go because the file and the tail
// are cut from the SAME bytes, and that is the property the omission markers
// rest on: the tail says "lines 41-8213 omitted", so line 8214 of this file has
// to be the line the model was shown next. Nothing is prepended to it for the
// same reason — not the command, not a timestamp, not a header — because a
// header of one line makes every number in every marker wrong by one.
//
// What the command was is in the audit and in the trace, where a reader can
// join it to this path.
func (r *Recorder) CheckLog(session string, attempt, index int, out string) (string, error) {
	if r == nil {
		return "", nil
	}
	dir := filepath.Join(r.dir, "checks")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	p := filepath.Join(dir, fmt.Sprintf("%s%s-%d.%d.log", session, r.checkRoundOf(dir, session), attempt, index+1))
	// Scrubbed HERE, like every other file lca writes, and not by the caller: the
	// caller's copy is what the MODEL is shown and has to say which header was
	// missing (see verify.go). The markers still index this file across that
	// difference, because [redacted] carries no newline — the two copies differ in
	// bytes and agree, line for line, on what line a line is.
	//
	// Through the string and not through []byte(out): on a stand this is the whole
	// of a five-minute deploy, and the boxing alone was measured at half a
	// gigabyte of garbage per attempt on top of a peak that already held the same
	// output three times over.
	//
	// 0600 and the same atomic write as the transcript: this holds whatever the
	// stand printed out of a repository that may be private, and a write that
	// fails halfway must not leave a file whose line numbers no longer match the
	// markers that point into it.
	if err := writeRecordAtomicString(p, redactSecrets(out), 0o600); err != nil {
		return "", err
	}
	return p, nil
}

// checkRoundOf is the "" of a first round and ".r2", ".r3" … of the rounds after
// it, and it exists because `-session <uid>` starts the attempt counter at 1
// again. Without it round two's first attempt writes the name round one's first
// attempt already has — and that path is in the JSON result round one handed the
// wrapper, with line numbers in its check_tail that index the bytes this write
// would replace. An operator opening check_logs[0] from the first round to find
// out why the first attempt failed would read the second round's log, with
// nothing anywhere saying so.
//
// Decided ONCE per process, on the first log written, by looking for a free
// name: every round writes attempt 1 before anything else, so the probe is
// deterministic and every later file of that round carries the same suffix. A
// delegated subagent writes under its own session id and so cannot collide with
// its lead, but it shares this answer, which is right — it is the same round.
//
// The suffix sits between the session and the attempt, so pruneCheckLogs still
// reads the session off the name and both rounds land in one session's slot.
func (r *Recorder) checkRoundOf(dir, session string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.checkRoundSet {
		return r.checkRound
	}
	r.checkRoundSet = true
	for round := 1; round <= maxCheckRounds; round++ {
		suffix := ""
		if round > 1 {
			suffix = fmt.Sprintf(".r%d", round)
		}
		if _, err := os.Stat(filepath.Join(dir, fmt.Sprintf("%s%s-1.1.log", session, suffix))); os.IsNotExist(err) {
			r.checkRound = suffix
			return suffix
		}
	}
	// A hundred rounds of one session is not a pipeline, and overwriting the
	// FIRST round is still the wrong answer, so the hundredth shares a name with
	// the ninety-ninth rather than with round one.
	r.checkRound = fmt.Sprintf(".r%d", maxCheckRounds)
	return r.checkRound
}

const (
	maxCheckRounds = 99
	// checkLogDirBytes bounds the directory in BYTES, which is the unit this
	// artefact is measured in: one attempt of a stand check was measured at 500 MB
	// and KeepSessions — 200, a number chosen for kilobyte transcripts — would
	// have retained around 200 GB of them before deleting a single file. A count
	// of sessions is the wrong bound for a megabytes-per-attempt file sitting next
	// to kilobyte transcripts, so both bounds apply and the tighter one wins.
	//
	// The NEWEST session is never collected by this one, however big it is: it is
	// the run whose result object just handed the wrapper these paths, and a
	// ceiling that deletes the evidence of the run that is being triaged is worse
	// than a full disk, which at least says so. So the directory holds the last
	// run plus this much history.
	checkLogDirBytes = 512 << 20
	// checkLogTmpAge is how old an interrupted write's leftover has to be before
	// the sweep takes it. A .lca-tmp-* of the same size as the log it was becoming
	// is invisible to a *.log sweep, so an interrupted run used to leave half a
	// gigabyte that nothing would ever collect — but another process may be
	// writing one right now, and a rename is not atomic with respect to a stat.
	checkLogTmpAge = time.Hour
)

// pruneCheckLogs collects the check logs of old sessions. `keep` is the same
// session count pruneTranscripts applies to the transcripts they sit beside, and
// `except` is the session a `-session <uid>` round is continuing, which is never
// collected — the whole point of that round is to carry on from what the first
// one wrote, and its paths are in the JSON the wrapper already recorded.
//
// By session and not by file, because one run writes one log per check per
// attempt and counting files would keep three attempts of one session and
// nothing else. A delegation's logs count as its lead's: `<uid>-t1` and
// `<uid>-t2` each took a slot of their own, so one lead run with ten subagents
// consumed eleven of the two hundred and evicted ten earlier runs whose
// transcripts survived them.
func pruneCheckLogs(dir string, keep int, except string) {
	paths, _ := checkLogsToCollect(dir, keep, except)
	for _, p := range paths {
		os.Remove(p)
	}
}

// checkLogsToCollect is the decision — which files, and how many bytes they are
// — separated from the deleting so that `lca clean --dry-run` can print exactly
// what the real sweep would do rather than a guess at it. It removes nothing
// itself.
func checkLogsToCollect(dir string, keep int, except string) ([]string, int64) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, 0
	}
	newest := map[string]int64{}
	owner := map[string][]string{}
	size := map[string]int64{}
	var litter []string
	litterBytes := int64(0)
	now := time.Now()
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		// An interrupted write's leftover, as big as the log it was becoming and
		// invisible to a *.log sweep. Old enough that nobody is still writing it:
		// a rename is not atomic with respect to a stat, and another process may be
		// mid-write right now.
		if strings.HasPrefix(e.Name(), ".lca-tmp-") {
			if now.Sub(info.ModTime()) > checkLogTmpAge {
				litter = append(litter, filepath.Join(dir, e.Name()))
				litterBytes += info.Size()
			}
			continue
		}
		if !strings.HasSuffix(e.Name(), ".log") {
			continue
		}
		sess := checkLogSession(e.Name())
		if sess == "" {
			continue // not a name CheckLog wrote: not ours, left alone
		}
		if ts := info.ModTime().UnixNano(); ts > newest[sess] {
			newest[sess] = ts
		}
		owner[sess] = append(owner[sess], filepath.Join(dir, e.Name()))
		size[sess] += info.Size()
	}
	if len(owner) == 0 {
		return litter, litterBytes
	}
	// Newest first. Both bounds walk this one order: the count takes everything
	// past `keep`, and the byte ceiling then takes from the far end inwards.
	sessions := sortedKeys(newest)
	for i := 1; i < len(sessions); i++ {
		for j := i; j > 0 && newest[sessions[j]] > newest[sessions[j-1]]; j-- {
			sessions[j], sessions[j-1] = sessions[j-1], sessions[j]
		}
	}
	collect := litter
	bytes := litterBytes
	drop := func(s string) {
		collect = append(collect, owner[s]...)
		bytes += size[s]
		delete(owner, s)
	}
	live := sessions
	if keep > 0 && len(sessions) > keep {
		for _, s := range sessions[keep:] {
			if s != except {
				drop(s)
			}
		}
		live = sessions[:keep]
	}
	total := int64(0)
	for _, s := range live {
		if _, ok := owner[s]; ok {
			total += size[s]
		}
	}
	// Oldest first, and never the last one standing: see checkLogDirBytes.
	for i := len(live) - 1; i > 0 && total > checkLogDirBytes; i-- {
		s := live[i]
		if s == except {
			continue
		}
		if _, ok := owner[s]; !ok {
			continue
		}
		total -= size[s]
		drop(s)
	}
	return collect, bytes
}

// checkLogSession is the session a check log belongs to, or "" for a name
// CheckLog did not write. It undoes the three things CheckLog and its callers
// put after the session id: the `-<attempt>.<index>.log` tail, the `.r<n>` of a
// second round, and the `-t<n>` / `-compact` of a delegated or compacting
// session — so every log of one run shares one slot whoever wrote it.
func checkLogSession(name string) string {
	i := strings.LastIndexByte(name, '-')
	if i <= 0 {
		return ""
	}
	s := name[:i]
	if j := strings.LastIndexByte(s, '.'); j > 0 && strings.HasPrefix(s[j:], ".r") {
		if _, err := strconv.Atoi(s[j+2:]); err == nil {
			s = s[:j]
		}
	}
	s = strings.TrimSuffix(s, "-compact")
	if j := strings.LastIndexByte(s, '-'); j > 0 && len(s) > j+2 && s[j+1] == 't' {
		if _, err := strconv.Atoi(s[j+2:]); err == nil {
			s = s[:j]
		}
	}
	return s
}

// dirBytes is what one flat directory holds, in bytes. Only files directly in
// it — the check logs are flat, and a surprise recursion here would make `lca
// clean` report a number for somebody else's tree.
func dirBytes(dir string) int64 {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	total := int64(0)
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if info, err := e.Info(); err == nil {
			total += info.Size()
		}
	}
	return total
}
