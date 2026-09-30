package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// The wire. JSON-RPC 2.0 over HTTP (POST, with the Streamable-HTTP/SSE reply
// shape) and over a child process's pipes.
//
// WHAT THIS IS NOT, so the next reader does not think something was forgotten:
//
//   - Implemented: initialize, notifications/initialized, tools/list,
//     tools/call. Four messages, and nothing else is ever sent.
//   - NOT implemented and never called: resources/*, prompts/*, completion/*,
//     logging/*, roots/*. We advertise `capabilities: {}` — nothing we do not
//     implement — so a server has been told not to expect them.
//   - No server→client requests (sampling/createMessage, elicitation/create,
//     roots/list). We never open the server's listening GET stream, so nothing
//     can arrive unbidden; a server-initiated request that turns up inside a
//     POST's own reply stream is dropped and counted, and /mcp shows the count.
//   - No marketplace, no discovery, no npx/uvx. An internal endpoint, or a
//     binary the operator already installed. Nothing else.

const (
	// The version we ask for. A server reporting something else is ACCEPTED and
	// displayed: refusing on a version string would break on the next spec
	// revision for no safety gain, since these four messages have not changed.
	mcpProtocolVersion = "2025-06-18"
	mcpClientVersion   = "0.1"

	maxMCPBytes          = 1 << 20 // 1 MiB read cap per reply
	mcpHandshakeTimeout  = 5 * time.Second
	maxMCPListPages      = 10
	maxMCPListTools      = 250
	mcpStderrRing        = 4 << 10
	mcpStdioScannerBytes = 4 << 20
)

type rpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	// nil means a notification: the field is omitted ENTIRELY and not sent as
	// null, because "id": null is a malformed request and some servers say so.
	ID     *int64 `json:"id,omitempty"`
	Method string `json:"method"`
	Params any    `json:"params,omitempty"`
}

type rpcError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      *int64          `json:"id"`
	Method  string          `json:"method"` // set only on a server→client message
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// mcpConn is one live connection. Both transports answer the same two questions,
// so the handshake and the call sites are written once.
type mcpConn interface {
	call(ctx context.Context, method string, params any) (json.RawMessage, error)
	notify(ctx context.Context, method string, params any) error
	label() string
	stderr() string
	droppedCount() int
	close() error
}

// ── HTTP ────────────────────────────────────────────────────────────────────

type httpConn struct {
	sv *MCPServer
	cl *http.Client

	// drift is set by MCPServer.connect for a connection a SESSION will use, so the
	// one-shot re-handshake below knows to re-compare the tool list. A probe and a
	// refresh leave it false on purpose: looking at a server whose tools have
	// changed is exactly what they are for.
	drift bool

	mu        sync.Mutex
	id        int64
	sessionID string
	proto     string
	retried   bool // one re-handshake after a 404, ever
	dropped   int
}

// transportFor builds the one *http.Transport a server may use. Egress is the
// part of this feature the security team will ask about, so it is decided here
// and nowhere else: a hostname on the operator's list, an address inside their
// networks, and no redirect that could move either.
//
// There is no insecure / skip_verify knob at any level, and adding one later
// should be refused in review: on a monitored corporate host, "the TLS error
// went away" must mean "the CA is configured", never "verification is off".
func (m *MCPSet) transportFor(sv *MCPServer) (*http.Transport, error) {
	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if sv.CAFile != "" {
		pool, err := x509.SystemCertPool()
		if err != nil || pool == nil {
			pool = x509.NewCertPool()
		}
		pem, err := os.ReadFile(sv.CAFile)
		if err != nil {
			return nil, fmt.Errorf("mcp server %q: ca_file %s: %w", sv.Name, sv.CAFile, err)
		}
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("mcp server %q: ca_file %s holds no PEM certificate", sv.Name, sv.CAFile)
		}
		tlsCfg.RootCAs = pool
	}
	// Control is closed over THIS server's host:port: the loopback exemption belongs
	// to the allowlist entry that earned it, not to every name sharing its port.
	d := &net.Dialer{Timeout: 10 * time.Second, Control: m.allow.controlFor(sv.Host)}
	return &http.Transport{
		DialContext:           d.DialContext,
		TLSClientConfig:       tlsCfg,
		MaxIdleConns:          2,
		IdleConnTimeout:       60 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 60 * time.Second,
	}, nil
}

func dialMCPHTTP(sv *MCPServer) (mcpConn, error) {
	// The name check again, right before connecting: the load-time check read the
	// file, and the file may have been reloaded since.
	if err := sv.set.allow.permitHost(sv.Host); err != nil {
		return nil, fmt.Errorf("mcp server %q: %w", sv.Name, err)
	}
	tr, err := sv.set.transportFor(sv)
	if err != nil {
		return nil, err
	}
	cl := &http.Client{
		Transport: tr,
		// No client Timeout: every call carries its own context deadline, and a
		// client-wide timeout would cut a legitimately slow tools/call.
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			// Every redirect, including a same-host one. A redirect is the cheapest
			// way to turn an allowlisted host into an unallowlisted one AND to leak
			// the Authorization header, which Go forwards on a same-host hop.
			return fmt.Errorf("mcp server %q: refusing a redirect to %s — an MCP endpoint that moves is a configuration change, not a runtime decision", sv.Name, req.URL.Redacted())
		},
	}
	return &httpConn{sv: sv, cl: cl}, nil
}

func (c *httpConn) label() string { return c.sv.URL }
func (c *httpConn) stderr() string {
	return "" // an HTTP server has no stderr of ours to drain
}

func (c *httpConn) droppedCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := c.dropped
	c.dropped = 0
	return n
}

func (c *httpConn) hasSession() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sessionID != ""
}

func (c *httpConn) setProto(v string) {
	c.mu.Lock()
	c.proto = v
	c.mu.Unlock()
}

func (c *httpConn) nextID() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.id++
	return c.id
}

// authHeaders resolves the operator's headers. A resolved value exists only
// inside this function and the *http.Request it fills; MCPServer has no field it
// could be stored in.
func (sv *MCPServer) authHeaders(h http.Header) error {
	for _, ref := range sv.Headers {
		if !ref.set() {
			return fmt.Errorf("mcp server %q: $%s is not set, so header %s cannot be built — export it, or lca would send a malformed credential", sv.Name, ref.EnvVar, ref.Name)
		}
		h.Set(ref.Name, ref.value())
	}
	return nil
}

func (c *httpConn) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	id := c.nextID()
	raw, err := c.post(ctx, &rpcRequest{JSONRPC: "2.0", ID: &id, Method: method, Params: params}, id)
	if err == nil {
		return raw, nil
	}
	var expired mcpSessionExpired
	if !errors.As(err, &expired) {
		return nil, err
	}
	// The server dropped our session. Re-handshake exactly once: a server that
	// restarts behind a proxy is ordinary, and a loop of handshakes is not.
	c.mu.Lock()
	already := c.retried
	c.retried, c.sessionID = true, ""
	c.mu.Unlock()
	if already {
		return nil, c.sessionGone()
	}
	if _, herr := mcpHandshake(ctx, c); herr != nil {
		return nil, c.sessionGone()
	}
	// The server that answered the second handshake may be a NEW process — a restart
	// behind a proxy is the ordinary reason for the 404 — and its tool list is
	// therefore unverified against the lock. connect() compares it for exactly this
	// reason; skipping it here let the rest of the session go on calling a tool whose
	// schema had moved, which is the one thing the frozen prefix promises cannot
	// happen quietly. Same check, same sentence, so one situation has one explanation.
	if c.drift {
		if err := c.sv.recheckDrift(ctx, c); err != nil {
			return nil, err
		}
	}
	id = c.nextID()
	raw, err = c.post(ctx, &rpcRequest{JSONRPC: "2.0", ID: &id, Method: method, Params: params}, id)
	if errors.As(err, &expired) {
		// The re-handshake worked and the session died again: that is a server we
		// cannot hold a session with, and retrying forever would only hide it.
		return nil, c.sessionGone()
	}
	return raw, err
}

func (c *httpConn) sessionGone() error {
	return fmt.Errorf("mcp server %q ended its MCP session (HTTP 404) and a second handshake also failed — run /mcp probe %s", c.sv.Name, c.sv.Name)
}

func (c *httpConn) notify(ctx context.Context, method string, params any) error {
	_, err := c.post(ctx, &rpcRequest{JSONRPC: "2.0", Method: method, Params: params}, 0)
	return err
}

// mcpSessionExpired is the one status worth a retry, carried as a type so call()
// does not have to match on a message.
type mcpSessionExpired struct{}

func (mcpSessionExpired) Error() string { return "the MCP session expired" }

func (c *httpConn) post(ctx context.Context, rq *rpcRequest, id int64) (json.RawMessage, error) {
	body, err := json.Marshal(rq)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.sv.URL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	c.mu.Lock()
	proto, sid := c.proto, c.sessionID
	c.mu.Unlock()
	if proto != "" && rq.Method != "initialize" {
		req.Header.Set("MCP-Protocol-Version", proto)
	}
	if sid != "" {
		req.Header.Set("Mcp-Session-Id", sid)
	}
	if err := c.sv.authHeaders(req.Header); err != nil {
		return nil, err
	}
	resp, err := c.cl.Do(req)
	if err != nil {
		return nil, fmt.Errorf("mcp server %q: %w", c.sv.Name, scrubErr(err, c.sv))
	}
	defer resp.Body.Close()

	if sid := resp.Header.Get("Mcp-Session-Id"); sid != "" {
		c.mu.Lock()
		c.sessionID = sid
		c.mu.Unlock()
	}
	switch {
	case resp.StatusCode == http.StatusNotFound && sid != "":
		return nil, mcpSessionExpired{}
	case resp.StatusCode == http.StatusAccepted, resp.StatusCode == http.StatusOK:
	default:
		// The first 200 bytes of the body, scrubbed: an internal reverse proxy's
		// HTML error page becomes readable instead of mysterious.
		peek, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
		return nil, fmt.Errorf("mcp server %q: HTTP %d %s", c.sv.Name, resp.StatusCode,
			scrubSecrets(strings.TrimSpace(collapseWS(string(peek))), c.sv.secrets()))
	}
	if rq.ID == nil {
		// A notification: 202 with an empty body is expected, 200 with a body is
		// accepted and the body discarded.
		io.Copy(io.Discard, io.LimitReader(resp.Body, maxMCPBytes))
		return nil, nil
	}
	if strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream") {
		return c.readSSE(resp.Body, id)
	}
	return c.readJSON(resp.Body, id)
}

func (c *httpConn) readJSON(r io.Reader, id int64) (json.RawMessage, error) {
	data, err := readCapped(r)
	if err != nil {
		return nil, fmt.Errorf("mcp server %q: %w", c.sv.Name, err)
	}
	var resp rpcResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("mcp server %q: its reply is not JSON-RPC: %s", c.sv.Name,
			truncate(scrubSecrets(collapseWS(string(data)), c.sv.secrets()), 200))
	}
	return c.match(&resp, id)
}

// readSSE reads the Streamable-HTTP reply shape: blank line ends a frame, data:
// lines join with \n, event: other than "message" is skipped, id:/retry: are
// ignored. The first frame carrying OUR id is the answer; the body is closed
// after it and the stream is not drained.
func (c *httpConn) readSSE(r io.Reader, id int64) (json.RawMessage, error) {
	// Counted, not just limited: a reply over the cap is cut mid-frame, the truncated
	// JSON fails to parse and the loop ends — which used to be reported as "its
	// stream ended without answering the request", blaming the server for a limit
	// lca imposed. The memory bound held either way; the sentence did not.
	cr := &countingReader{r: io.LimitReader(r, maxMCPBytes+1)}
	sc := bufio.NewScanner(cr)
	sc.Buffer(make([]byte, 0, 64<<10), mcpStdioScannerBytes)
	var data []string
	event := "message"
	flush := func() (json.RawMessage, bool, error) {
		if len(data) == 0 {
			event = "message"
			return nil, false, nil
		}
		payload := strings.Join(data, "\n")
		data = nil
		want := event == "" || event == "message"
		event = "message"
		if !want {
			return nil, false, nil
		}
		var resp rpcResponse
		if err := json.Unmarshal([]byte(payload), &resp); err != nil {
			return nil, false, nil // not ours to understand; skip the frame
		}
		raw, err := c.match(&resp, id)
		if err != nil {
			if errors.Is(err, errNotOurID) {
				return nil, false, nil
			}
			return nil, true, err
		}
		return raw, true, nil
	}
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		switch {
		case line == "":
			raw, done, err := flush()
			if done || err != nil {
				return raw, err
			}
		case strings.HasPrefix(line, ":"): // a comment / keep-alive
		case strings.HasPrefix(line, "data:"):
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		case strings.HasPrefix(line, "event:"):
			event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("mcp server %q: reading its stream: %w", c.sv.Name, scrubErr(err, c.sv))
	}
	if cr.n > maxMCPBytes {
		return nil, fmt.Errorf("mcp server %q: %s", c.sv.Name, mcpTooBig())
	}
	// A final frame with no trailing blank line.
	if raw, done, err := flush(); done || err != nil {
		return raw, err
	}
	return nil, fmt.Errorf("mcp server %q: its stream ended without answering the request", c.sv.Name)
}

var errNotOurID = errors.New("not our id")

// match turns one decoded JSON-RPC message into a result, an error, or "skip".
func (c *httpConn) match(resp *rpcResponse, id int64) (json.RawMessage, error) {
	if resp.Method != "" {
		// A server→client message. A request (it has an id) is dropped and counted:
		// we advertise no capabilities, so there is nothing we could answer.
		if resp.ID != nil {
			c.mu.Lock()
			c.dropped++
			c.mu.Unlock()
		}
		return nil, errNotOurID
	}
	if resp.ID == nil || *resp.ID != id {
		return nil, errNotOurID
	}
	if resp.Error != nil {
		return nil, &mcpRPCError{server: c.sv.Name, err: *resp.Error}
	}
	return resp.Result, nil
}

func (c *httpConn) close() error {
	c.mu.Lock()
	sid := c.sessionID
	c.sessionID = ""
	c.mu.Unlock()
	if sid != "" {
		// Best effort, 2s, errors ignored: a server that does not implement DELETE
		// is not a problem worth a message at shutdown.
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if req, err := http.NewRequestWithContext(ctx, http.MethodDelete, c.sv.URL, nil); err == nil {
			req.Header.Set("Mcp-Session-Id", sid)
			c.sv.authHeaders(req.Header)
			if resp, err := c.cl.Do(req); err == nil {
				io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
				resp.Body.Close()
			}
		}
	}
	c.cl.CloseIdleConnections()
	return nil
}

// mcpRPCError is a JSON-RPC error from the server. It is a distinct type from a
// tool-level isError, because they are different faults and get different
// wording.
type mcpRPCError struct {
	server string
	err    rpcError
}

func (e *mcpRPCError) Error() string {
	return fmt.Sprintf("mcp server %q: the server refused the call (%d): %s", e.server, e.err.Code, e.err.Message)
}

// ── stdio ───────────────────────────────────────────────────────────────────

type stdioConn struct {
	sv     *MCPServer
	cmd    *exec.Cmd
	in     io.WriteCloser
	sc     *bufio.Scanner
	ring   *ringBuf
	cancel context.CancelFunc
	lbl    string

	// callMu serializes a whole call. One pipe is one conversation: two Parallel
	// reads sharing this connection would each write a request and then race to
	// read, and readUntil skips a reply whose id is not its own — so one goroutine
	// would DISCARD the other's answer and the other would hang until its deadline.
	callMu sync.Mutex

	mu      sync.Mutex
	id      int64
	dropped int
	dead    bool
}

// dialMCPStdio spawns the server. Five gates stand between a config file and a
// running program, and then an honest admission in §residual risks: the spawned
// server is a child process, not a jailed one. Jail confines the paths and
// commands of lca's OWN tools; it cannot confine a program it started.
func dialMCPStdio(sv *MCPServer) (mcpConn, error) {
	m := sv.set
	if !m.stdio {
		return nil, fmt.Errorf("mcp server %q: stdio servers are disabled (mcp.stdio is \"deny\") — set \"stdio\": \"allow\" in the mcp block if you mean it", sv.Name)
	}
	// Gate 2, at spawn and not at registration, because the jail does not exist
	// when MCP registers. doctor checks it up front and names it.
	argv0 := filepath.Base(sv.Command)
	if m.jl != nil && !m.jl.AllowCommand(argv0) {
		return nil, fmt.Errorf("mcp server %q: command %q is not on the sandbox allowlist — add it to sandbox.allow in roles.yaml, or use an http transport", sv.Name, argv0)
	}
	ctx, cancel := context.WithCancel(m.ctx)
	// Gate 3: never a shell, whatever sandbox.shell says. A server definition is
	// not a command a model wrote and must not inherit the looser semantics.
	cmd := exec.CommandContext(ctx, sv.Command, sv.Args...)
	// Gate 4: cmd.Env from scratch, so a server cannot inherit LCA_GW_TOKEN by
	// accident — only the variables env: names.
	env := []string{}
	for _, k := range []string{"PATH", "HOME", "LANG", "TZ"} {
		if v := os.Getenv(k); v != "" {
			env = append(env, k+"="+v)
		}
	}
	for _, ref := range sv.Env {
		if !ref.set() {
			cancel()
			return nil, fmt.Errorf("mcp server %q: $%s is not set, so env %s cannot be built — export it", sv.Name, ref.EnvVar, ref.Name)
		}
		env = append(env, ref.Name+"="+ref.value())
	}
	cmd.Env = env
	if m.jl != nil {
		cmd.Dir = m.jl.Root
	}
	// Gate 5's other half: its own process group, so cancellation reaps
	// grandchildren too.
	inProcessGroup(cmd)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return nil, err
	}
	ring := newRingBuf(mcpStderrRing)
	errPipe, err := cmd.StderrPipe()
	if err != nil {
		cancel()
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		cancel()
		return nil, fmt.Errorf("mcp server %q: %w", sv.Name, err)
	}
	// stderr into a bounded ring and surfaced only in /mcp and doctor, never into
	// a tool result: a server that prints a token to stderr must not get it into
	// the transcript.
	go func() { io.Copy(ring, errPipe) }()

	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 0, 64<<10), mcpStdioScannerBytes)
	return &stdioConn{sv: sv, cmd: cmd, in: stdin, sc: sc, ring: ring, cancel: cancel,
		lbl: strings.Join(append([]string{sv.Command}, sv.Args...), " ")}, nil
}

func (c *stdioConn) label() string  { return c.lbl }
func (c *stdioConn) stderr() string { return c.ring.String() }
func (c *stdioConn) droppedCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := c.dropped
	c.dropped = 0
	return n
}
func (c *stdioConn) notify(ctx context.Context, method string, params any) error {
	return c.write(&rpcRequest{JSONRPC: "2.0", Method: method, Params: params})
}

func (c *stdioConn) write(rq *rpcRequest) error {
	body, err := json.Marshal(rq)
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.dead {
		return fmt.Errorf("mcp server %q: its process is gone%s", c.sv.Name, c.tailNote())
	}
	if _, err := c.in.Write(append(body, '\n')); err != nil {
		return fmt.Errorf("mcp server %q: writing to it: %v%s", c.sv.Name, err, c.tailNote())
	}
	return nil
}

func (c *stdioConn) tailNote() string {
	if t := strings.TrimSpace(c.ring.String()); t != "" {
		return " (it said: " + truncate(collapseWS(scrubSecrets(t, c.sv.secrets())), 200) + ")"
	}
	return ""
}

func (c *stdioConn) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	c.callMu.Lock()
	defer c.callMu.Unlock()
	c.mu.Lock()
	c.id++
	id := c.id
	c.mu.Unlock()
	if err := c.write(&rpcRequest{JSONRPC: "2.0", ID: &id, Method: method, Params: params}); err != nil {
		return nil, err
	}
	type reply struct {
		raw json.RawMessage
		err error
	}
	ch := make(chan reply, 1)
	go func() {
		raw, err := c.readUntil(id)
		ch <- reply{raw, err}
	}()
	select {
	case r := <-ch:
		return r.raw, r.err
	case <-ctx.Done():
		// A cancelled read leaves a reply in the pipe that would answer the NEXT
		// call, so the connection cannot be reused: tear the child down instead of
		// keeping a desynchronized stream.
		c.kill()
		return nil, ctx.Err()
	}
}

func (c *stdioConn) readUntil(id int64) (json.RawMessage, error) {
	for c.sc.Scan() {
		line := bytes.TrimSpace(c.sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var resp rpcResponse
		if err := json.Unmarshal(line, &resp); err != nil {
			continue // a server that prints prose on stdout: not a message
		}
		if resp.Method != "" {
			if resp.ID != nil {
				c.mu.Lock()
				c.dropped++
				c.mu.Unlock()
			}
			continue
		}
		if resp.ID == nil || *resp.ID != id {
			continue
		}
		if resp.Error != nil {
			return nil, &mcpRPCError{server: c.sv.Name, err: *resp.Error}
		}
		return resp.Result, nil
	}
	err := c.sc.Err()
	c.mu.Lock()
	note := c.tailNote()
	c.mu.Unlock()
	if err != nil {
		return nil, fmt.Errorf("mcp server %q: reading from it: %v%s", c.sv.Name, err, note)
	}
	return nil, fmt.Errorf("mcp server %q: it closed its output without answering%s", c.sv.Name, note)
}

// kill and close are the same act — this child is finished — and both go through
// teardown, because exec.Cmd.Wait is not safe to call twice: kill() used to run it
// unguarded, so a shutdown racing an in-flight call's cancellation could have two
// goroutines reading and writing cmd.ProcessState at once, with the loser getting
// ECHILD. The `dead` flag decides which caller does the work; the other returns.
func (c *stdioConn) kill() { c.teardown() }

func (c *stdioConn) close() error {
	c.teardown()
	return nil
}

func (c *stdioConn) teardown() {
	c.mu.Lock()
	already := c.dead
	c.dead = true
	c.mu.Unlock()
	if already {
		return
	}
	c.in.Close()
	// A well-behaved server exits when its stdin closes; the cancel is what
	// guarantees it and reaps the whole process group either way.
	c.cancel()
	c.cmd.Wait()
}

// ringBuf keeps the LAST n bytes written to it: a server's stderr tail is what
// says "JIRA_TOKEN is not set", and the tail is where that lives.
type ringBuf struct {
	mu   sync.Mutex
	buf  []byte
	max  int
	over bool
}

func newRingBuf(max int) *ringBuf { return &ringBuf{max: max} }

func (r *ringBuf) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.buf = append(r.buf, p...)
	if len(r.buf) > r.max {
		r.buf = r.buf[len(r.buf)-r.max:]
		r.over = true
	}
	return len(p), nil
}

func (r *ringBuf) String() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := string(r.buf)
	if r.over {
		return "…" + s
	}
	return s
}

// ── the four messages ───────────────────────────────────────────────────────

type mcpServerInfo struct {
	Protocol  string
	Name      string
	Version   string
	SessionID bool
	NoTools   bool // the server reports no tools capability
}

type mcpRawTool struct {
	Name        string         `json:"name"`
	Title       string         `json:"title"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
	Annotations *struct {
		ReadOnlyHint    *bool `json:"readOnlyHint"`
		DestructiveHint *bool `json:"destructiveHint"`
	} `json:"annotations"`
}

// mcpHandshake sends initialize and the notification that completes it.
func mcpHandshake(ctx context.Context, c mcpConn) (mcpServerInfo, error) {
	var info mcpServerInfo
	raw, err := c.call(ctx, "initialize", map[string]any{
		"protocolVersion": mcpProtocolVersion,
		"capabilities":    map[string]any{}, // nothing we do not implement
		"clientInfo":      map[string]any{"name": "lca", "version": mcpClientVersion},
	})
	if err != nil {
		return info, err
	}
	var r struct {
		ProtocolVersion string `json:"protocolVersion"`
		Capabilities    struct {
			Tools json.RawMessage `json:"tools"`
		} `json:"capabilities"`
		ServerInfo struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"serverInfo"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return info, fmt.Errorf("its initialize result is not what MCP describes: %w", err)
	}
	info.Protocol, info.Name, info.Version = r.ProtocolVersion, r.ServerInfo.Name, r.ServerInfo.Version
	// A server reporting no tools capability is usable-but-empty and contributes
	// nothing. We do not guess that it has tools anyway.
	info.NoTools = len(r.Capabilities.Tools) == 0
	if p, ok := c.(*httpConn); ok {
		p.setProto(info.Protocol)
		info.SessionID = p.hasSession()
	}
	if err := c.notify(ctx, "notifications/initialized", nil); err != nil {
		return info, fmt.Errorf("its handshake could not be completed: %w", err)
	}
	return info, nil
}

// mcpListTools follows nextCursor, bounded. Past either bound the list is
// truncated and says so, in the lock, in /mcp and in doctor: the fix is naming
// fewer tools in expose:, not a knob.
//
// This runs only during refresh, probe, doctor and the first call of a session
// (the drift check) — never to build a prefix.
func mcpListTools(ctx context.Context, c mcpConn) ([]mcpRawTool, bool, error) {
	var out []mcpRawTool
	cursor := ""
	for page := 0; page < maxMCPListPages; page++ {
		params := map[string]any{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		raw, err := c.call(ctx, "tools/list", params)
		if err != nil {
			return nil, false, err
		}
		var r struct {
			Tools      []mcpRawTool `json:"tools"`
			NextCursor string       `json:"nextCursor"`
		}
		if err := json.Unmarshal(raw, &r); err != nil {
			return nil, false, fmt.Errorf("its tools/list result is not what MCP describes: %w", err)
		}
		out = append(out, r.Tools...)
		if len(out) >= maxMCPListTools {
			return out[:maxMCPListTools], true, nil
		}
		if r.NextCursor == "" {
			return out, false, nil
		}
		cursor = r.NextCursor
	}
	return out, true, nil
}

// mcpCallTool calls one tool and renders its content.
//
// structuredContent is deliberately ignored: it duplicates the text content on
// every server that sends it, and handing the model two spellings of one answer
// is how a turn spends its context twice.
func mcpCallTool(ctx context.Context, c mcpConn, tool string, args map[string]any) (string, error) {
	if args == nil {
		args = map[string]any{}
	}
	raw, err := c.call(ctx, "tools/call", map[string]any{"name": tool, "arguments": args})
	if err != nil {
		return "", err
	}
	var r struct {
		Content []struct {
			Type     string `json:"type"`
			Text     string `json:"text"`
			MimeType string `json:"mimeType"`
			Data     string `json:"data"`
			Resource *struct {
				URI  string `json:"uri"`
				Text string `json:"text"`
			} `json:"resource"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return "", fmt.Errorf("its tools/call result is not what MCP describes: %w", err)
	}
	var parts []string
	for _, p := range r.Content {
		switch {
		case p.Type == "text":
			parts = append(parts, p.Text)
		case p.Resource != nil:
			s := p.Resource.URI
			if p.Resource.Text != "" {
				s += "\n" + p.Resource.Text
			}
			parts = append(parts, s)
		default:
			// Base64 bytes never enter a transcript: they would cost the context
			// window a picture the model cannot see anyway.
			parts = append(parts, fmt.Sprintf("[%s %s %s, not shown]", orElseStr(p.Type, "content"),
				byteCount(len(p.Data)*3/4), orElseStr(p.MimeType, "unknown type")))
		}
	}
	text := strings.Join(parts, "\n")
	if r.IsError {
		return "", fmt.Errorf("%s failed: %s", tool, truncate(collapseWS(text), 400))
	}
	return text, nil
}

func orElseStr(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}

// ── leak prevention ─────────────────────────────────────────────────────────

// scrubSecrets replaces a resolved secret with [redacted]. Every string leaving
// the MCP layer toward the model, the trace, the audit log or the screen passes
// through it, because a 401 body or a chatty stdio server can echo a token back
// and we must not rely on it not doing so.
func scrubSecrets(s string, secrets []string) string {
	for _, sec := range secrets {
		if len(sec) < 4 { // a one-character "secret" would redact the whole message
			continue
		}
		s = strings.ReplaceAll(s, sec, "[redacted]")
	}
	return s
}

func scrubErr(err error, sv *MCPServer) error {
	if err == nil {
		return nil
	}
	if s := scrubSecrets(err.Error(), sv.secrets()); s != err.Error() {
		return errors.New(s)
	}
	return err
}

// readCapped reads at most maxMCPBytes and says so when there was more — a
// server that answers with a 5 MB ticket dump must produce a message, not a
// memory spike.
func readCapped(r io.Reader) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, maxMCPBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxMCPBytes {
		return nil, errors.New(mcpTooBig())
	}
	return data, nil
}

// mcpTooBig is one sentence, so both reply shapes report the limit identically.
func mcpTooBig() string {
	return fmt.Sprintf("its reply is larger than %s, which lca will not read into memory", byteCount(maxMCPBytes))
}

// countingReader counts what was actually consumed, which is how readSSE tells
// "the stream ended" from "lca stopped reading at the cap".
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// collapseWS folds a multi-line error body onto one line, so an HTML error page
// becomes a readable sentence instead of forty lines of chrome.
func collapseWS(s string) string { return strings.Join(strings.Fields(s), " ") }
