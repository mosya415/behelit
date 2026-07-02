package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Native Slurm endpoint discovery — a Go port of the modelstat pipeline, so lca
// needs no external tool. For each RUNNING job (scoped to a reservation) we ask
// scontrol for the authoritative facts (serving node = BatchHost, gpu count, log
// paths, sbatch Command), read the job log on shared NFS to find the port the
// server bound to ("Uvicorn running on http://…:PORT" — both vLLM and sglang use
// uvicorn), fall back to the container startup script when the log lacks it, and
// finally confirm health + model over HTTP. Needs only squeue/scontrol + NFS
// reads + HTTP from the login/dev node — no compute-node access.

const (
	discoverLogMaxBytes = 4_000_000
	discoverConcurrency = 12
	probeTimeout        = 2 * time.Second
	slurmTimeout        = 10 * time.Second
)

var (
	reUvicorn     = regexp.MustCompile(`[Uu]vicorn running on https?://[^\s:/]+:(\d+)`)
	reArgPort     = regexp.MustCompile(`(?:--port[ =]|\bport[=:])\s*(\d+)`)
	reArgModel    = regexp.MustCompile(`(?:--model(?:-path)?[ =]|\bmodel[=:])\s*['"]?([^\s'"]+)`)
	reScriptPort  = regexp.MustCompile(`--port[ =]+(\d+)`)
	reScriptModel = regexp.MustCompile(`--model(?:-path)?[ =]+['"]?([^\s'"\\]+)`)
	reMounts      = regexp.MustCompile(`(?:--container-mounts=|--mount[ =]|(?:^|\s)-m[ =]+)["']?([^"'\s]+)`)
	reShPath      = regexp.MustCompile(`(/[^\s"']*\.sh)`)
	reGres        = regexp.MustCompile(`gres/gpu[:=](\d+)`)
)

// msModel is one discovered inference endpoint.
type msModel struct {
	Node        string
	Port        int
	Health      string // up | unhealthy | unknown | down
	Engine      string // sglang | vllm
	Model       string // served id / path
	ServedNames []string
	MaxModelLen int
	GpuCount    int
	JobID       string
	Err         string // why the port/endpoint could not be resolved
}

type msResult struct {
	Models   []msModel
	Warnings []string
}

// Endpoint is "host:port", or "" when the port could not be found.
func (m msModel) Endpoint() string {
	if m.Port == 0 || m.Node == "" {
		return ""
	}
	return fmt.Sprintf("%s:%d", m.Node, m.Port)
}

func (m msModel) baseURL(scheme string) string {
	if scheme == "" {
		scheme = "http"
	}
	return fmt.Sprintf("%s://%s:%d/v1", scheme, m.Node, m.Port)
}

// modelName is what to send in the chat `model` field.
func (m msModel) modelName() string {
	if len(m.ServedNames) > 0 && m.ServedNames[0] != "" {
		return m.ServedNames[0]
	}
	return m.Model
}

// display is a short label (basename of a model path).
func (m msModel) display() string {
	if len(m.ServedNames) > 0 && m.ServedNames[0] != "" {
		return m.ServedNames[0]
	}
	if m.Model == "" {
		return "(unknown)"
	}
	return path.Base(m.Model)
}

func healthGlyph(h string) string {
	switch h {
	case "up":
		return cGreen + gUp + cReset
	case "unhealthy":
		return cYellow + gPartial + cReset
	case "down":
		return cRed + gDown + cReset
	default:
		return cFaint + gNone + cReset
	}
}

// readFileHead reads up to maxBytes from the top of a file (NFS logs can be huge).
func readFileHead(pathStr string, maxBytes int) (string, error) {
	f, err := os.Open(pathStr)
	if err != nil {
		return "", err
	}
	defer f.Close()
	buf := make([]byte, maxBytes)
	n, err := io.ReadFull(f, buf)
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return "", err
	}
	return string(buf[:n]), nil
}

type slurmJob struct {
	id, name, user, state, gres string
	nodes                       []string
}

// runTool executes a read-only Slurm/CLI command with a timeout, returning stdout.
func runTool(name string, args []string, timeout time.Duration) (string, error) {
	if _, err := exec.LookPath(name); err != nil {
		return "", fmt.Errorf("%s not found — run lca on a Slurm login/dev node", name)
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, name, args...).Output()
	return string(out), err
}

func squeueJobs(reservation, user string) ([]slurmJob, error) {
	args := []string{"--noheader", "--states=RUNNING", "--format=%i|%j|%u|%T|%N|%M|%b|%P"}
	if reservation != "" {
		args = append(args, "--reservation", reservation)
	}
	if user != "" {
		args = append(args, "--user", user)
	}
	out, err := runTool("squeue", args, slurmTimeout)
	if err != nil {
		return nil, fmt.Errorf("squeue failed: %w", err)
	}
	var jobs []slurmJob
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		f := strings.Split(line, "|")
		if len(f) < 8 {
			continue
		}
		gres := f[6]
		if gres == "N/A" {
			gres = ""
		}
		jobs = append(jobs, slurmJob{
			id: f[0], name: f[1], user: f[2], state: f[3],
			nodes: expandNodelist(f[4]), gres: gres,
		})
	}
	return jobs, nil
}

func expandNodelist(nl string) []string {
	nl = strings.TrimSpace(nl)
	if nl == "" || nl == "(null)" || nl == "None" {
		return nil
	}
	if !strings.ContainsAny(nl, "[,") {
		return []string{nl}
	}
	if out, err := runTool("scontrol", []string{"show", "hostnames", nl}, 5*time.Second); err == nil {
		var names []string
		for _, n := range strings.Split(out, "\n") {
			if n = strings.TrimSpace(n); n != "" {
				names = append(names, n)
			}
		}
		if len(names) > 0 {
			return names
		}
	}
	return strings.Split(nl, ",")
}

// scontrolShow parses `scontrol show job <id>` into key=value (first wins).
func scontrolShow(jobID string) map[string]string {
	out, err := runTool("scontrol", []string{"show", "job", jobID}, 5*time.Second)
	if err != nil {
		return map[string]string{}
	}
	data := map[string]string{}
	for _, tok := range strings.Fields(out) {
		if i := strings.IndexByte(tok, '='); i > 0 {
			k := tok[:i]
			if _, ok := data[k]; !ok {
				data[k] = tok[i+1:]
			}
		}
	}
	return data
}

func gresGpuCount(sc map[string]string) int {
	for _, key := range []string{"AllocTRES", "ReqTRES", "TresPerNode"} {
		if m := reGres.FindStringSubmatch(sc[key]); m != nil {
			n, _ := strconv.Atoi(m[1])
			return n
		}
	}
	return 0
}

func gpuFromGres(gres string) int {
	if gres == "" {
		return 0
	}
	tail := gres[strings.LastIndexByte(gres, ':')+1:]
	n, _ := strconv.Atoi(tail)
	return n
}

// scanLog reads a job log (bounded, from the top) for the bound port, engine and
// model. The bind banner is near the top, so we stop as soon as a port is found.
func scanLog(pathStr string) (port int, engine, model string) {
	data, err := readFileHead(pathStr, discoverLogMaxBytes)
	if err != nil {
		return 0, "", ""
	}
	argPort := 0
	for _, line := range strings.Split(data, "\n") {
		low := strings.ToLower(line)
		if engine == "" {
			if strings.Contains(low, "sglang") {
				engine = "sglang"
			} else if strings.Contains(low, "vllm") {
				engine = "vllm"
			}
		}
		if m := reUvicorn.FindStringSubmatch(line); m != nil {
			port, _ = strconv.Atoi(m[1])
		}
		if argPort == 0 {
			if m := reArgPort.FindStringSubmatch(line); m != nil {
				argPort, _ = strconv.Atoi(m[1])
			}
		}
		if model == "" {
			if m := reArgModel.FindStringSubmatch(line); m != nil && strings.Contains(m[1], "/") {
				model = m[1]
			}
		}
		if port != 0 || argPort != 0 {
			break
		}
	}
	if port == 0 {
		port = argPort
	}
	return port, engine, model
}

// resolveLaunchScript finds the startup .sh an sbatch runs and maps its
// container path back to the host (NFS) path via the mount specs.
func resolveLaunchScript(sbatchPath string) string {
	text, err := readFileHead(sbatchPath, discoverLogMaxBytes)
	if err != nil {
		return ""
	}
	var mounts [][2]string // {containerPrefix, hostPrefix}
	shpath := ""
	for _, raw := range strings.Split(text, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		for _, m := range reMounts.FindAllStringSubmatch(line, -1) {
			for _, pair := range strings.Split(m[1], ",") {
				p := strings.SplitN(pair, ":", 2)
				if len(p) == 2 && p[0] != "" && p[1] != "" {
					mounts = append(mounts, [2]string{
						strings.TrimRight(p[1], "/") + "/",
						strings.TrimRight(p[0], "/") + "/",
					})
				}
			}
		}
		if shpath == "" {
			if m := reShPath.FindStringSubmatch(line); m != nil {
				shpath = m[1]
			}
		}
	}
	if shpath == "" {
		return ""
	}
	for _, cm := range mounts {
		if strings.HasPrefix(shpath, cm[0]) {
			return cm[1] + shpath[len(cm[0]):]
		}
	}
	return shpath
}

// parseLaunchScript reads a startup script for (port, engine, model).
func parseLaunchScript(pathStr string) (port int, engine, model string) {
	text, err := readFileHead(pathStr, discoverLogMaxBytes)
	if err != nil {
		return 0, "", ""
	}
	for _, raw := range strings.Split(text, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		low := strings.ToLower(line)
		if engine == "" {
			if strings.Contains(low, "sglang") {
				engine = "sglang"
			} else if strings.Contains(low, "vllm") {
				engine = "vllm"
			}
		}
		if port == 0 {
			if m := reScriptPort.FindStringSubmatch(line); m != nil {
				port, _ = strconv.Atoi(m[1])
			}
		}
		if model == "" {
			if m := reScriptModel.FindStringSubmatch(line); m != nil {
				model = m[1][strings.LastIndexByte(m[1], '/')+1:] // basename
			}
		}
	}
	return port, engine, model
}

// probe confirms health and enriches model metadata over HTTP.
func probe(node string, port int, scheme string) (health string, served []string, maxLen int, engine string) {
	base := fmt.Sprintf("%s://%s:%d", scheme, node, port)

	code, _, ok := httpGet(base + "/health")
	switch {
	case !ok:
		if _, _, ok2 := httpGet(base + "/v1/models"); !ok2 {
			return "down", nil, 0, ""
		}
		health = "unknown"
	case code >= 200 && code < 300:
		health = "up"
	default:
		health = "unhealthy"
	}

	if c, body, ok := httpGet(base + "/v1/models"); ok && c >= 200 && c < 300 {
		var out modelsResponse
		if json.Unmarshal([]byte(body), &out) == nil {
			for _, d := range out.Data {
				if d.ID != "" {
					served = append(served, d.ID)
				}
				if maxLen == 0 && d.MaxModelLen > 0 {
					maxLen = d.MaxModelLen
				}
				if engine == "" {
					if o := strings.ToLower(d.OwnedBy); o == "sglang" || o == "vllm" {
						engine = o
					}
				}
			}
		}
	}
	return health, served, maxLen, engine
}

func httpGet(url string) (int, string, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, "", false
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, "", false
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, string(body), true
}

// discoverSlurm runs the whole pipeline, one endpoint per running job.
func discoverSlurm(reservation, user, scheme string) (msResult, error) {
	jobs, err := squeueJobs(reservation, user)
	if err != nil {
		return msResult{}, err
	}
	var res msResult
	if reservation == "" {
		res.Warnings = append(res.Warnings, "no reservation set (LCA_RESERVATION) — scanning all running jobs")
	}
	if len(jobs) == 0 {
		return res, nil
	}

	models := make([]msModel, len(jobs))
	sem := make(chan struct{}, discoverConcurrency)
	var wg sync.WaitGroup
	for i, job := range jobs {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, job slurmJob) {
			defer wg.Done()
			defer func() { <-sem }()
			models[i] = discoverOne(job, scheme)
		}(i, job)
	}
	wg.Wait()
	res.Models = models
	return res, nil
}

func discoverOne(job slurmJob, scheme string) msModel {
	sc := scontrolShow(job.id)
	node := sc["BatchHost"]
	if node == "" && len(job.nodes) > 0 {
		node = job.nodes[0]
	}
	gpu := gresGpuCount(sc)
	if gpu == 0 {
		gpu = gpuFromGres(job.gres)
	}

	var port int
	var engine, model string
	// 1) job logs (stderr first — the bind banner usually goes there)
	for _, key := range []string{"StdErr", "StdOut"} {
		if lp := sc[key]; lp != "" {
			p, e, mo := scanLog(lp)
			if engine == "" {
				engine = e
			}
			if model == "" {
				model = mo
			}
			if p != 0 {
				port = p
				break
			}
		}
	}
	// 2) fall back to the container startup script the sbatch runs
	errMsg := ""
	if port == 0 {
		if cmd := sc["Command"]; cmd != "" {
			if script := resolveLaunchScript(cmd); script != "" {
				p, e, mo := parseLaunchScript(script)
				if port == 0 {
					port = p
				}
				if engine == "" {
					engine = e
				}
				if model == "" {
					model = mo
				}
				if port == 0 {
					errMsg = "no --port in startup script"
				}
			} else {
				errMsg = "no launch script found in sbatch"
			}
		}
	}
	if engine == "" {
		low := strings.ToLower(sc["Command"])
		if strings.Contains(low, "sglang") {
			engine = "sglang"
		} else if strings.Contains(low, "vllm") {
			engine = "vllm"
		}
	}

	m := msModel{
		Node: node, Port: port, Engine: engine, Model: model,
		GpuCount: gpu, JobID: job.id, Health: "unknown",
	}
	if port == 0 {
		if errMsg == "" {
			errMsg = "serving port not found"
		}
		m.Err = errMsg
		m.Health = "down"
		return m
	}
	// 3) confirm over HTTP
	h, served, maxLen, eng := probe(node, port, scheme)
	m.Health = h
	m.ServedNames = served
	m.MaxModelLen = maxLen
	if eng != "" {
		m.Engine = eng
	}
	return m
}
