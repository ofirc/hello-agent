// Package sandbox runs model-written Python/bash inside Cloud Run sandboxes, or
// locally in Docker containers (see docker.go).
//
// The isolation comes entirely from the argument list in commandArgs: the
// sandbox sees the host filesystem read-only, has no network and no env vars
// unless flags say otherwise. So the list must never grow --allow-egress,
// --env, --write, or a second mount; the one bind mount is the per-run data
// directory, read-only.
package sandbox

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

type Result struct {
	Stdout, Stderr string
	ExitCode       int
	TimedOut       bool
}

type Runner interface {
	// Name is the backend, "cloudrun" or "docker", for doctor and the startup log.
	Name() string
	Available(ctx context.Context) bool
	Run(ctx context.Context, lang, code, dataDir string) (Result, error)
}

type CloudRun struct {
	Bin       string        // default "/usr/local/gcp/bin/sandbox"
	Timeout   time.Duration // default 60 * time.Second
	MaxOutput int           // default 16384

	once  sync.Once
	avail bool
	// probeTimeout bounds the startup probe; 0 means defaultProbeTimeout. Tests
	// shorten it.
	probeTimeout time.Duration
	// name is "shim" when Bin is the local stand-in, so doctor and the log can say the
	// runs are not isolated. Empty means the real Cloud Run CLI.
	name string
}

func (c *CloudRun) Name() string {
	if c.name != "" {
		return c.name
	}
	return "cloudrun"
}

const defaultProbeTimeout = 10 * time.Second

const probeFile, probeValue = "probe.txt", "sandbox-probe-ok"

// cloudRunBin is a var so Select's tests do not depend on the machine.
var cloudRunBin = "/usr/local/gcp/bin/sandbox"

func NewCloudRun() *CloudRun {
	return &CloudRun{
		Bin:       cloudRunBin,
		Timeout:   60 * time.Second,
		MaxOutput: 16384,
	}
}

// NewShim is the Cloud Run runner driving deploy/local/sandbox-shim instead of the
// real CLI. Arguments, timeouts, truncation and the probe are exactly production's;
// the isolation is not there at all, because the shim just execs. Local use only.
func NewShim(cli string) *CloudRun {
	c := NewCloudRun()
	c.Bin = cli
	c.name = "shim"
	return c
}

// codeSeq keeps concurrent runs that share a dataDir from clobbering each
// other's script files.
var codeSeq atomic.Uint64

const truncSuffix = "\n...truncated" // stdout: head kept
const truncPrefix = "...truncated\n" // stderr: tail kept

// killGrace bounds how long Wait lingers on output pipes after the kill: a
// grandchild that escaped the process group would otherwise hold them open.
const killGrace = 500 * time.Millisecond

// cliEnv is the sandbox CLI's whole environment. The bot's process holds the
// Slack, GitHub and JIRA tokens; the platform does not forward env vars into
// the sandbox without --env, but that is the CLI's behaviour, not ours, so the
// tokens never reach the CLI in the first place.
var cliEnv = []string{"PATH=/usr/local/bin:/usr/bin:/bin"}

// Available reports whether the sandbox CLI exists and can run a Python
// program that reads a file from its mounted data directory. Probed once: the
// binary does not appear or vanish while the process is running, and a probe
// per tool call would be wasted latency. The probe detaches from ctx's
// cancellation: the result is cached for the process lifetime, so one caller's
// already-cancelled context must not disable the tool for everyone.
func (c *CloudRun) Available(ctx context.Context) bool {
	c.once.Do(func() { c.avail = c.probe(context.WithoutCancel(ctx)) })
	return c.avail
}

// probe goes through Run, over a directory made the way a run's is (MkdirTemp,
// so 0700, with a 0644 file), so it shares Run's process-group kill and
// WaitDelay and cannot block startup past its timeout, and so a sandbox user
// that cannot read the mount shows as unavailable instead of failing every
// run_code call.
func (c *CloudRun) probe(ctx context.Context) bool {
	if _, err := os.Stat(c.Bin); err != nil {
		return false
	}
	dir, err := os.MkdirTemp("", "sandbox-probe-*")
	if err != nil {
		return false
	}
	defer os.RemoveAll(dir)
	if err := os.WriteFile(filepath.Join(dir, probeFile), []byte(probeValue), 0o644); err != nil {
		return false
	}
	timeout := c.probeTimeout
	if timeout == 0 {
		timeout = defaultProbeTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	res, err := c.Run(ctx, "python", "print(open('/data/"+probeFile+"').read())", dir)
	return err == nil && !res.TimedOut && res.ExitCode == 0 && res.Stdout == probeValue+"\n"
}

func (c *CloudRun) Run(ctx context.Context, lang, code, dataDir string) (Result, error) {
	interp, err := interpreterFor(lang)
	if err != nil {
		return Result{}, err
	}
	file, err := writeCodeFile(dataDir, lang, code)
	if err != nil {
		return Result{}, err
	}
	defer os.Remove(filepath.Join(dataDir, file))

	runCtx, cancel := context.WithTimeout(ctx, c.Timeout)
	defer cancel()

	// Tracebacks sit at the end of stderr, so it keeps its tail; stdout keeps its head.
	stdout := &capBuffer{max: c.MaxOutput}
	stderr := &capBuffer{max: c.MaxOutput, keepTail: true}
	cmd := exec.CommandContext(runCtx, c.Bin, commandArgs(dataDir, interp, file)...)
	cmd.Stdout, cmd.Stderr = stdout, stderr
	cmd.Env = cliEnv
	// The sandbox CLI may fork; killing only its pid would orphan the script.
	setProcessGroup(cmd)
	cmd.Cancel = func() error { return killProcessGroup(cmd) }
	cmd.WaitDelay = killGrace

	err = cmd.Run()
	res := Result{Stdout: stdout.String(), Stderr: stderr.String()}

	if ctx.Err() != nil {
		return res, ctx.Err()
	}
	if runCtx.Err() != nil {
		res.TimedOut = true
		res.ExitCode = -1
		return res, nil
	}
	var exitErr *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &exitErr):
		res.ExitCode = exitErr.ExitCode()
	default:
		return res, fmt.Errorf("run sandbox: %w", err)
	}
	return res, nil
}

func interpreterFor(lang string) (string, error) {
	switch lang {
	case "python":
		return "/usr/bin/python3", nil
	case "bash":
		return "/usr/bin/bash", nil
	}
	return "", fmt.Errorf("unsupported language %q (want python or bash)", lang)
}

// writeCodeFile puts the script into dataDir, which runners mount at /data, and
// returns its name there. The caller removes it.
func writeCodeFile(dataDir, lang, code string) (string, error) {
	ext := "py"
	if lang == "bash" {
		ext = "sh"
	}
	file := fmt.Sprintf(".code-%d.%s", codeSeq.Add(1), ext)
	if err := os.WriteFile(filepath.Join(dataDir, file), []byte(code), 0o644); err != nil {
		return "", fmt.Errorf("write code file: %w", err)
	}
	return file, nil
}

// commandArgs is the whole isolation policy; see the package comment.
func commandArgs(dataDir, interpreter, file string) []string {
	return []string{
		"do",
		"--mount", "type=bind,source=" + dataDir + ",destination=/data,readonly",
		"--",
		interpreter, "/data/" + file,
	}
}

// capBuffer bounds memory against a script that prints in a loop. It keeps
// either the first max bytes (head) or the last max bytes (keepTail).
type capBuffer struct {
	max       int
	keepTail  bool
	buf       []byte
	truncated bool
}

func (b *capBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if b.keepTail {
		b.buf = append(b.buf, p...)
		if over := len(b.buf) - b.max; over > 0 {
			b.buf = append(b.buf[:0], b.buf[over:]...)
			b.truncated = true
		}
		return n, nil
	}
	if room := b.max - len(b.buf); len(p) > room {
		p = p[:max(room, 0)]
		b.truncated = true
	}
	b.buf = append(b.buf, p...)
	return n, nil
}

func (b *capBuffer) String() string {
	switch {
	case !b.truncated:
		return string(b.buf)
	case b.keepTail:
		return truncPrefix + string(b.buf)
	}
	return string(b.buf) + truncSuffix
}
