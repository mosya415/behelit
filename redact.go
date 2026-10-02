package main

import (
	"os"
	"strings"
	"sync"
	"unicode/utf8"
)

// Secrets, out of the two artefacts that leave this machine.
//
// MCP responses are already scrubbed against the tokens the MCP layer itself
// resolved (mcpclient.go's scrubSecrets). What that cannot see is a token this
// process never handled: a `check.sh` that echoes its environment when it fails,
// a test that prints the failing request with its Authorization header, a deploy
// script that logs `curl -H "Authorization: Bearer $BSK_TOKEN"` before dying.
// The check's output is not ours and we cannot ask it to behave.
//
// So the environment is the source of truth: every variable whose NAME says it
// holds a credential has its VALUE replaced wherever it appears. The name
// patterns are the requirement's own — *TOKEN*, *KEY*, *SECRET*, *PASSWORD*.
//
// The two sinks this is wired into are the ones that go somewhere a wider
// audience reads: the summary file, which the wrapper pastes into a merge
// request description, and the result object's check_tail and reason, which it
// pastes into a ticket comment. Both are published by a program that will not
// read them first.

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

// envSecrets is the values to hide, longest first so that a token which
// contains a shorter one is replaced whole rather than left with its tail
// showing. Read once: the environment of a run does not change under it, and
// every line of a 20 000-line check output passes through here.
func envSecrets() []string {
	secretsOnce.Do(func() {
		for _, kv := range os.Environ() {
			i := strings.IndexByte(kv, '=')
			if i <= 0 {
				continue
			}
			name, val := kv[:i], kv[i+1:]
			if len(val) < minSecretLen || !looksSecretName(name) {
				continue
			}
			secretVals = append(secretVals, val)
		}
		// Longest first.
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

// redactSecrets replaces every known credential value with [redacted].
func redactSecrets(s string) string {
	if s == "" {
		return s
	}
	for _, v := range envSecrets() {
		if strings.Contains(s, v) {
			s = strings.ReplaceAll(s, v, "[redacted]")
		}
	}
	return s
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
