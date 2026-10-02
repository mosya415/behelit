package main

import (
	"encoding/json"
	"io"
	"os"
	"strings"
	"sync"
	"unicode/utf8"
)

// Secrets, out of every artefact this run writes.
//
// MCP responses are already scrubbed against the tokens the MCP layer itself
// resolved (mcpclient.go's scrubSecrets). What that cannot see is a token this
// process never handled: a `check.sh` that echoes its environment when it fails,
// a test that prints the failing request with its Authorization header, a deploy
// script that logs `curl -H "Authorization: Bearer $BSK_TOKEN"` before dying.
// The check's output is not ours and we cannot ask it to behave.
//
// So the environment is the source of truth: a variable whose NAME says it holds
// a credential and whose VALUE looks like one has that value replaced wherever
// it appears. The name patterns are the requirement's own — *TOKEN*, *KEY*,
// *SECRET*, *PASSWORD* — and they are substring matches on four very common
// English words, which is why the value is asked as well (looksSecretValue): a
// name alone redacted `API_KEY_HEADER=Authorization` out of the one line that
// said which header was missing.
//
// This started at the two sinks a wrapper pastes somewhere public — the summary
// file and the result object — and that was too narrow by exactly the artefact
// the requirement names: the TRANSCRIPT goes to Jira as an attachment, and with
// it the trace and the HTML report. So the scrub now sits at the write boundary
// of every one of them (recorder.go's Transcript, ChildTranscript and Event,
// trace.go's write, checktail.go's CheckLog, report.go's writeReport), where it
// cannot be forgotten by a new field: whatever a record carries, the bytes go
// through here on the way to the disk.
//
// A write boundary and not the source, deliberately. Redacting a tool RESULT
// would change what the model is shown, and a run that legitimately prints a
// token to a log it then greps would silently stop working. What a model sees
// is this program's business; what lands in a file somebody attaches to a ticket
// is everybody's.
//
// The cost is measured rather than assumed, because this runs on every write of
// a file that is rewritten after every turn. With no credential in the
// environment — every development machine — it is one nil check and not a byte
// is copied. With credentials set, on a 2.1 MB transcript with four spellings to
// look for: 3-4 ms per write, against json.MarshalIndent's own 2.8 ms on the
// same bytes. So the scrub roughly doubles the cost of writing a transcript, and
// that is the honest figure rather than a reassuring one: it is single-digit
// milliseconds on a write that already happens once per turn, next to a turn
// that costs seconds of gateway time. TestRedactionCostOnALongTranscript prints
// it, so nobody has to take this comment's word for it.

// secretNameParts are the name fragments that mean "this value is a
// credential". Matched case-insensitively against the whole variable name, so
// BSK_TOKEN, GITLAB_API_KEY and jira_password all match.
var secretNameParts = []string{"token", "key", "secret", "password", "passwd", "credential"}

// minSecretLen is the shortest value worth hiding. A two-character
// *_KEY=on would otherwise redact every "on" in a build log, which destroys the
// output the model and the reviewer need without protecting anything: a secret
// that short is not one.
const minSecretLen = 6

var (
	secretsOnce sync.Once
	secretVals  []string
)

// envSecrets is every spelling of every value to hide, longest first so that a
// token which contains a shorter one is replaced whole rather than left with its
// tail showing. Read once: the environment of a run does not change under it,
// and every line of a 20 000-line check output passes through here.
//
// Every SPELLING, because the artefacts are not plain text. A transcript and a
// trace record are JSON and a report is HTML, and both escape some bytes on the
// way out: a token containing a quote is written `\"` in the transcript and
// `&#34;` in the report, and a scrub that knew only the raw spelling would walk
// straight past it. Scrubbing the structs before they are marshalled would mean
// naming every field of every record, and the field somebody forgets is the one
// that leaks — so the scrub sits on the BYTES, and the bytes are searched for
// each value as the writers spell it.
//
// Dedup keeps that cheap: a token of letters, digits, dashes and underscores —
// which is nearly all of them — escapes to itself in both, so the three
// spellings collapse to one and a scan costs what it always did.
func envSecrets() []string {
	secretsOnce.Do(func() {
		env := os.Environ()
		// The names, as a set, for looksSecretValue's last question: a variable
		// whose value is another variable's NAME (`BSK_API_KEY_ENV=BSK_TOKEN`, the
		// indirection a dozen tools use) must not redact that name everywhere it
		// appears, including in the error that says which variable was missing.
		names := make(map[string]bool, len(env))
		for _, kv := range env {
			if i := strings.IndexByte(kv, '='); i > 0 {
				names[kv[:i]] = true
			}
		}
		for _, kv := range env {
			i := strings.IndexByte(kv, '=')
			if i <= 0 {
				continue
			}
			name, val := kv[:i], kv[i+1:]
			if !looksSecretName(name) || !looksSecretValue(val, names) {
				continue
			}
			for _, sp := range []string{val, jsonInner(val), esc(val)} {
				if sp != "" && !contains(secretVals, sp) {
					secretVals = append(secretVals, sp)
				}
			}
		}
		// Longest first — which also puts an escaped spelling, never shorter than
		// the raw one it came from, ahead of the raw one it contains.
		for i := 1; i < len(secretVals); i++ {
			for j := i; j > 0 && len(secretVals[j]) > len(secretVals[j-1]); j-- {
				secretVals[j], secretVals[j-1] = secretVals[j-1], secretVals[j]
			}
		}
	})
	return secretVals
}

func looksSecretName(name string) bool {
	low := strings.ToLower(name)
	for _, p := range secretNameParts {
		if strings.Contains(low, p) {
			return true
		}
	}
	return false
}

// The ceilings in looksSecretValue. A word is a word however long it is, but
// "however long" has to stop somewhere or a passphrase of five English words
// would walk through; 24 bytes covers the ones that actually turn up in a
// variable with KEY or TOKEN in its name (Authorization, Bearer, strong,
// disabled) and leaves `correcthorsebatterystaple` on the secret side. The
// identifier ceiling is lower because letters-and-digits is the shape of a real
// token as well as of `ed25519` and `utf8`: twelve bytes of it is not a
// credential anybody would issue, and above that the doubt goes the other way.
const (
	maxSecretWordLen  = 24
	maxSecretIdentLen = 12
)

// looksSecretValue is the second half of the test, and the half that was
// missing: the NAME says the variable may hold a credential, and this says
// whether the VALUE is one.
//
// It exists because the name patterns are substring matches on four very common
// English words, and because of where the scrub reaches. `API_KEY_HEADER=
// Authorization` is an ordinary variable in the operator's own environment, and
// with no test on the value the check's own failure line — `request rejected:
// missing Authorization header` — reached the model as `missing [redacted]
// header`: it cannot see which header, cannot fix it, and spends every attempt
// guessing. `TOKENIZER_PATH=/models/qwen3-coder-30b/tokenizer.json` on an
// inference box, `GITLAB_TOKEN_FILE=/run/secrets/gitlab_token`,
// `TOKEN_PREFIX=Bearer`, `SSH_KEY_ALGO=ed25519` and `BSK_API_KEY_ENV=BSK_TOKEN`
// all did the same to the artefacts a human then reads.
//
// So four things a credential is not, and the costs are not symmetric either
// way: a false positive destroys the text the model and the reviewer work from,
// a false negative leaves a credential in a ticket. These four are the shapes
// where a value is a name, a path or a word rather than something issued —
// anything with punctuation, a separator or real length in it still goes.
//
// names is the environment's own key set, passed in so this stays a pure
// function of its arguments and testable without touching the process.
func looksSecretValue(val string, names map[string]bool) bool {
	if len(val) < minSecretLen {
		return false
	}
	// A filesystem path. The value is where to FIND the credential, not the
	// credential — and the path then appears in every `cannot open …: permission
	// denied` the operator has to read.
	switch {
	case val[0] == '/' || val[0] == '~':
		return false
	case strings.HasPrefix(val, "./") || strings.HasPrefix(val, "../"):
		return false
	case len(val) > 2 && val[1] == ':' && (val[2] == '\\' || val[2] == '/'):
		return false // C:\… — a Windows path
	}
	// The name of another variable, which is how a tool is told WHERE its
	// credential is. Either spelled like one and shouting, or literally one of
	// this process's own.
	if names[val] {
		return false
	}
	digits, under, lower := 0, 0, 0
	for i := 0; i < len(val); i++ {
		switch c := val[i]; {
		case c >= 'a' && c <= 'z':
			lower++
		case c >= 'A' && c <= 'Z':
		case c >= '0' && c <= '9':
			digits++
		case c == '_':
			under++
		default:
			// Punctuation, a space, a dash, a slash, a non-ASCII byte: issued things
			// look like this and words do not, so nothing below can excuse it.
			return true
		}
	}
	if under > 0 && lower == 0 {
		return false // SHOUTING_SNAKE_CASE: a variable name, not its value
	}
	switch {
	case digits == 0 && under == 0 && len(val) <= maxSecretWordLen:
		return false // a word: Authorization, Bearer, strong
	case under == 0 && len(val) <= maxSecretIdentLen:
		return false // a short identifier: ed25519, utf8mb4
	}
	return true
}

// redactSecrets replaces every known credential value with [redacted].
func redactSecrets(s string) string {
	if s == "" {
		return s
	}
	for _, v := range envSecrets() {
		if strings.Contains(s, v) {
			s = strings.ReplaceAll(s, v, redactedMark)
		}
	}
	return s
}

const redactedMark = "[redacted]"

// jsonInner is how encoding/json spells s inside a string, without the quotes:
// the transcript and the trace are written by a json encoder, so this is the
// form a secret actually takes in them.
func jsonInner(s string) string {
	b, err := json.Marshal(s)
	if err != nil || len(b) < 2 {
		return s
	}
	return string(b[1 : len(b)-1])
}

// longestRedaction bounds how much of a stream has to be held back to catch a
// secret that straddles two writes. 0 means there is nothing to look for.
func longestRedaction() int {
	r := envSecrets()
	if len(r) == 0 {
		return 0
	}
	return len(r[0]) // sorted longest first
}

// redactBytes is redactSecrets for the bytes of a record on their way to a
// file. It returns b itself when there is nothing to hide, which is the whole
// of the cost on a machine with no credentials in its environment.
func redactBytes(b []byte) []byte {
	all := envSecrets()
	if len(all) == 0 || len(b) == 0 {
		return b
	}
	s := string(b)
	for _, v := range all {
		if strings.Contains(s, v) {
			s = strings.ReplaceAll(s, v, redactedMark)
		}
	}
	return []byte(s)
}

// redactWriter is redactBytes for something written in pieces — the report,
// which is rendered straight into a file through a 64 KiB buffer and never
// exists as one string.
//
// It holds back the last (longest secret) bytes of every write, because a
// token can straddle the boundary between two of them: anything that begins
// early enough to be complete in the buffer is scrubbed and flushed, and
// anything that begins later waits for the bytes that would finish it. Flush
// releases the tail, so it has to be called before the file is renamed into
// place.
type redactWriter struct {
	w    io.Writer
	hold []byte
	max  int
}

func newRedactWriter(w io.Writer) *redactWriter {
	return &redactWriter{w: w, max: longestRedaction()}
}

func (rw *redactWriter) Write(p []byte) (int, error) {
	if rw.max == 0 { // nothing to look for: a straight pass-through
		return rw.w.Write(p)
	}
	n := len(p)
	rw.hold = redactBytes(append(rw.hold, p...))
	if len(rw.hold) <= rw.max {
		return n, nil
	}
	cut := len(rw.hold) - rw.max
	if _, err := rw.w.Write(rw.hold[:cut]); err != nil {
		return 0, err
	}
	// Copied, not resliced: append above may reuse this array, and a hold that
	// aliased bytes already written would duplicate them on the next write.
	rw.hold = append([]byte(nil), rw.hold[cut:]...)
	return n, nil
}

// Flush writes the held tail. Writing nothing is the common case and must stay
// cheap: a report on a machine with no secrets never allocates a hold at all.
func (rw *redactWriter) Flush() error {
	if len(rw.hold) == 0 {
		return nil
	}
	_, err := rw.w.Write(redactBytes(rw.hold))
	rw.hold = nil
	return err
}

// forPublication is redactSecrets plus the two things that make a byte stream
// unsafe to paste: terminal escapes, which a build tool emits whether or not
// anybody is watching, and C0 control bytes, which a NUL in the middle of a
// Jira comment turns into a rejected API call. Tabs and newlines stay — they are
// the text's own shape.
//
// The result is also valid UTF-8, because the one thing a wrapper does with this
// is hand it to a JSON encoder or to Python's open(encoding="utf-8"): a check
// command that printed one byte of latin-1 must not be able to fail the step
// that posts the comment.
func forPublication(s string) string {
	s = stripANSI(redactSecrets(s))
	if !utf8.ValidString(s) {
		s = strings.ToValidUTF8(s, "�")
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < 0x20 && c != '\n' && c != '\t' || c == 0x7f {
			continue
		}
		b.WriteByte(c)
	}
	return b.String()
}
