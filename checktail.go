package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
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
//     nothing is lost and a person (or the model, with read_file and grep) can
//     go and look. Its path is in the result object.
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

// checkTailOf is what the model is shown of a check's output.
//
// Short output is returned UNCHANGED — byte for byte, no markers, no
// selection. That is not an optimisation: `go test ./...` on a red package
// prints forty lines, all of them load-bearing, and an operator reading
// check_tail in the result object should see what the command printed and not
// an edited version of it. The threshold is the old tail's own, so every output
// that used to arrive whole still does.
func checkTailOf(out string) string {
	out = strings.TrimRight(out, "\n")
	if out == "" {
		return ""
	}
	lines := strings.Split(out, "\n")
	if len(lines) <= tailLines && len(out) <= tailBytes {
		return out
	}

	keep := make([]bool, len(lines))
	spent := 0
	// The last forty first, and before any budget is spent elsewhere: they are
	// the harness's verdict, and a selection that reported the middle and lost
	// the summary would have moved the problem rather than solved it.
	for i := len(lines) - 1; i >= 0 && i >= len(lines)-checkTailLast; i-- {
		cost := lineCost(lines[i])
		if spent+cost > checkTailBytes && i < len(lines)-1 {
			break // one enormous line in the epilogue must not cost the whole budget
		}
		keep[i] = true
		spent += cost
	}

	// Then the matching regions, from the FIRST forwards. In a build log the
	// first error is the cause and the rest are its consequences — a failed
	// compile followed by two hundred "unresolved symbol" lines — so when the
	// budget runs out it is the consequences that are dropped.
	dropped := 0
	for _, r := range matchRegions(lines) {
		cost := 0
		for i := r.from; i <= r.to; i++ {
			if !keep[i] {
				cost += lineCost(lines[i])
			}
		}
		if spent+cost > checkTailBytes {
			dropped++
			continue
		}
		for i := r.from; i <= r.to; i++ {
			keep[i] = true
		}
		spent += cost
	}

	var b strings.Builder
	gap := 0
	for i, l := range lines {
		if !keep[i] {
			gap++
			continue
		}
		if gap > 0 {
			// 1-based and inclusive, so the numbers are the ones `sed -n` and
			// `grep -n` print for the full output file: the marker is an index into
			// the file, which is the only reason to print numbers at all.
			fmt.Fprintf(&b, "[… lines %d-%d omitted of %d …]\n", i-gap+1, i, len(lines))
			gap = 0
		}
		b.WriteString(clipLine(l))
		b.WriteByte('\n')
	}
	if gap > 0 {
		fmt.Fprintf(&b, "[… lines %d-%d omitted of %d …]\n", len(lines)-gap+1, len(lines), len(lines))
	}
	out = strings.TrimRight(b.String(), "\n")
	if dropped > 0 {
		// Said out loud, because the model is about to be asked to fix the cause:
		// "there are more of these" is the difference between it fixing one failure
		// and it believing it has seen them all.
		out += fmt.Sprintf("\n[… %s more matching the same words is in the full output …]",
			plural(dropped, "region", "regions"))
	}
	return out
}

// matchRegions is each matching line with checkTailContext lines on either
// side, merged where they overlap, in file order.
func matchRegions(lines []string) []lineSpan {
	var out []lineSpan
	for i, l := range lines {
		if !marksAFailure(l) {
			continue
		}
		from, to := i-checkTailContext, i+checkTailContext
		if from < 0 {
			from = 0
		}
		if to > len(lines)-1 {
			to = len(lines) - 1
		}
		if n := len(out); n > 0 && from <= out[n-1].to+1 {
			out[n-1].to = to
			continue
		}
		out = append(out, lineSpan{from, to})
	}
	return out
}

// marksAFailure is the per-line test. It lowercases once and asks four
// questions, because this runs on every line of every attempt of a check that
// may have printed twenty thousand of them.
func marksAFailure(line string) bool {
	if strings.Contains(line, "✗") {
		return true
	}
	low := strings.ToLower(line)
	for _, m := range checkTailMarks {
		if m != "✗" && strings.Contains(low, m) {
			return true
		}
	}
	return false
}

func lineCost(l string) int {
	if len(l) > checkTailLineBytes {
		return checkTailLineBytes + 1
	}
	return len(l) + 1
}

// clipLine cuts the middle out of one absurdly long line, keeping both ends:
// the start of a line is what names it and the end is where the assertion
// message usually is.
func clipLine(l string) string {
	if len(l) <= checkTailLineBytes {
		return l
	}
	head, tail := checkTailLineBytes*2/3, checkTailLineBytes/3
	// Cut on rune boundaries, so a Cyrillic message or a UTF-8 path is not left
	// half-encoded in a field a JSON encoder is about to be handed. At most three
	// bytes either way: that is the longest a truncated rune can be.
	h := l[:head]
	for i := 0; i < 3 && !utf8.ValidString(h); i++ {
		h = h[:len(h)-1]
	}
	tl := l[len(l)-tail:]
	for i := 0; i < 3 && !utf8.ValidString(tl); i++ {
		tl = tl[1:]
	}
	return fmt.Sprintf("%s…[%d bytes cut]…%s", h, len(l)-len(h)-len(tl), tl)
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
	p := filepath.Join(dir, fmt.Sprintf("%s-%d.%d.log", session, attempt, index+1))
	// 0600 and the same atomic write as the transcript: this holds whatever the
	// stand printed out of a repository that may be private, and a write that
	// fails halfway must not leave a file whose line numbers no longer match the
	// markers that point into it.
	if err := writeRecordAtomic(p, []byte(out), 0o600); err != nil {
		return "", err
	}
	return p, nil
}

// pruneCheckLogs keeps the logs of the `keep` most recent SESSIONS and deletes
// the rest, which is the same bound pruneTranscripts applies to the transcripts
// they sit beside — by session and not by file, because one run writes one log
// per check per attempt and counting files would keep three attempts of one
// session and nothing else.
//
// A stand's output is megabytes, and an unattended pipeline runs every few
// minutes: without this, the one artefact that is written on every single
// attempt is also the only one that grows for ever.
func pruneCheckLogs(dir string, keep int) {
	if keep <= 0 {
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	// The session is the name up to the last "-<attempt>.<index>.log", which is
	// exactly what CheckLog appended; a file that does not have that shape is
	// not ours and is left alone.
	newest := map[string]int64{}
	owner := map[string][]string{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".log") {
			continue
		}
		i := strings.LastIndexByte(e.Name(), '-')
		if i <= 0 {
			continue
		}
		sess := e.Name()[:i]
		info, err := e.Info()
		if err != nil {
			continue
		}
		if ts := info.ModTime().UnixNano(); ts > newest[sess] {
			newest[sess] = ts
		}
		owner[sess] = append(owner[sess], filepath.Join(dir, e.Name()))
	}
	if len(owner) <= keep {
		return
	}
	sessions := sortedKeys(newest)
	// Newest first, then everything past `keep` goes.
	for i := 1; i < len(sessions); i++ {
		for j := i; j > 0 && newest[sessions[j]] > newest[sessions[j-1]]; j-- {
			sessions[j], sessions[j-1] = sessions[j-1], sessions[j]
		}
	}
	for _, s := range sessions[keep:] {
		for _, p := range owner[s] {
			os.Remove(p)
		}
	}
}
