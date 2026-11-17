package sandbox

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestCommandArgsExactShape(t *testing.T) {
	got := commandArgs("/tmp/sre-runs/abc", "/usr/bin/python3", ".code-1.py")
	want := []string{
		"do", "--mount", "type=bind,source=/tmp/sre-runs/abc,destination=/data,readonly",
		"--", "/usr/bin/python3", "/data/.code-1.py",
	}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("args = %q, want %q", got, want)
	}
}

func TestCommandArgsNeverRelaxIsolation(t *testing.T) {
	args := commandArgs("/tmp/x", "/usr/bin/bash", ".code-2.sh")
	mounts := 0
	for _, a := range args {
		switch a {
		case "--allow-egress", "--env", "-e", "--write":
			t.Errorf("args contain %q: %q", a, args)
		case "--mount":
			mounts++
		}
	}
	if mounts != 1 {
		t.Errorf("want exactly one --mount, got %d: %q", mounts, args)
	}
}

func TestInterpreterFor(t *testing.T) {
	for lang, want := range map[string]string{"python": "/usr/bin/python3", "bash": "/usr/bin/bash"} {
		got, err := interpreterFor(lang)
		if err != nil || got != want {
			t.Errorf("interpreterFor(%q) = %q, %v; want %q", lang, got, err, want)
		}
	}
	if _, err := interpreterFor("ruby"); err == nil {
		t.Error("interpreterFor(ruby) should fail")
	}
}

// fakeSandbox stands in for the real CLI, which is absent on dev machines: it
// maps the --mount source onto /data and runs the file with the host shell,
// ignoring the interpreter path (/usr/bin/bash does not exist on macOS).
func fakeSandbox(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "sandbox")
	script := `#!/bin/sh
src=
while [ $# -gt 0 ]; do
  case "$1" in
    --mount) src=${2#*source=}; src=${src%%,*}; shift 2 ;;
    --) shift; break ;;
    *) shift ;;
  esac
done
# remaining: <interpreter> /data/<file>
exec /bin/sh "$src/${2#/data/}"
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin
}

func TestRunWithFakeBinary(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake sandbox is a POSIX shell script")
	}
	r := NewCloudRun()
	r.Bin = fakeSandbox(t)
	// Generous: only the timeout case may expire, or a slow spawn under load flakes the rest.
	r.Timeout = 10 * time.Second

	t.Run("stdout", func(t *testing.T) {
		res, err := r.Run(context.Background(), "bash", "echo hi", t.TempDir())
		if err != nil || res.Stdout != "hi\n" || res.ExitCode != 0 || res.TimedOut {
			t.Fatalf("res = %+v, err = %v", res, err)
		}
	})

	t.Run("nonzero exit is not an error", func(t *testing.T) {
		res, err := r.Run(context.Background(), "bash", "echo oops >&2; exit 3", t.TempDir())
		if err != nil || res.ExitCode != 3 || res.Stderr != "oops\n" {
			t.Fatalf("res = %+v, err = %v", res, err)
		}
	})

	t.Run("stdout keeps head and stderr keeps tail", func(t *testing.T) {
		code := `echo HEAD; head -c 20000 /dev/zero | tr '\0' x; echo END
echo HEAD >&2; head -c 20000 /dev/zero | tr '\0' x >&2; echo END >&2`
		res, err := r.Run(context.Background(), "bash", code, t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		const suffix, prefix = "\n...truncated", "...truncated\n"
		if len(res.Stdout) != 16384+len(suffix) || !strings.HasPrefix(res.Stdout, "HEAD\n") ||
			!strings.HasSuffix(res.Stdout, suffix) || strings.Contains(res.Stdout, "END") {
			t.Errorf("stdout not head-truncated: len=%d, got %.20q ... %.20q", len(res.Stdout), res.Stdout, tailOf(res.Stdout, 20))
		}
		if len(res.Stderr) != len(prefix)+16384 || !strings.HasPrefix(res.Stderr, prefix) ||
			!strings.HasSuffix(res.Stderr, "END\n") || strings.Contains(res.Stderr, "HEAD") {
			t.Errorf("stderr not tail-truncated: len=%d, got %.20q ... %.20q", len(res.Stderr), res.Stderr, tailOf(res.Stderr, 20))
		}
	})

	t.Run("timeout kills the process group", func(t *testing.T) {
		pidFile := filepath.Join(t.TempDir(), "pid")
		short := NewCloudRun()
		short.Bin = r.Bin
		short.Timeout = 200 * time.Millisecond
		start := time.Now()
		// The backgrounded sleep is a grandchild: only a group kill reaches it.
		res, err := short.Run(context.Background(), "bash", "sleep 30 & echo $! > "+pidFile+"; wait", t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		if !res.TimedOut {
			t.Fatalf("TimedOut = false: %+v", res)
		}
		if d := time.Since(start); d > 2*time.Second {
			t.Fatalf("took %v, want < 2s", d)
		}
		raw, err := os.ReadFile(pidFile)
		if err != nil {
			t.Fatalf("script never recorded grandchild pid: %v", err)
		}
		pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { syscall.Kill(pid, syscall.SIGKILL) }) // don't leak on failure
		deadline := time.Now().Add(2 * time.Second)
		for syscall.Kill(pid, 0) != syscall.ESRCH {
			if time.Now().After(deadline) {
				t.Fatalf("grandchild %d survived the timeout kill", pid)
			}
			time.Sleep(20 * time.Millisecond)
		}
	})

	t.Run("code file is cleaned up", func(t *testing.T) {
		dir := t.TempDir()
		if _, err := r.Run(context.Background(), "bash", "true", dir); err != nil {
			t.Fatal(err)
		}
		if left, _ := filepath.Glob(filepath.Join(dir, ".code-*")); len(left) != 0 {
			t.Fatalf("leftover code files: %v", left)
		}
	})

	t.Run("unknown language", func(t *testing.T) {
		if _, err := r.Run(context.Background(), "ruby", "puts 1", t.TempDir()); err == nil {
			t.Fatal("want error")
		}
	})
}

// The tokens the bot holds must not reach the sandbox CLI, whatever it forwards.
func TestRunPassesNoHostEnvironment(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake sandbox is a POSIX shell script")
	}
	t.Setenv("FAKE_SECRET_TOKEN", "x")
	bin := filepath.Join(t.TempDir(), "sandbox")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nenv\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	r := NewCloudRun()
	r.Bin = bin
	r.Timeout = 10 * time.Second
	res, err := r.Run(context.Background(), "python", "print(1)", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(res.Stdout, "TOKEN") {
		t.Fatalf("sandbox CLI saw a token variable:\n%s", res.Stdout)
	}
	if !strings.Contains(res.Stdout, "PATH=/usr/local/bin:/usr/bin:/bin") {
		t.Fatalf("sandbox CLI env lacks the fixed PATH:\n%s", res.Stdout)
	}
}

func TestRunMissingBinaryErrors(t *testing.T) {
	r := NewCloudRun()
	r.Bin = "/nonexistent"
	if _, err := r.Run(context.Background(), "bash", "true", t.TempDir()); err == nil {
		t.Fatal("want error")
	}
}

func TestAvailableFalseWhenBinaryMissing(t *testing.T) {
	r := NewCloudRun()
	r.Bin = "/nonexistent"
	if r.Available(context.Background()) {
		t.Fatal("Available = true for a missing binary")
	}
}

// The probe runs at startup before Slack connects, so a sandbox CLI whose child keeps
// stdout open must not hold the bot past the probe timeout plus the kill grace.
func TestAvailableDoesNotHangOnAStuckChild(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake sandbox is a POSIX shell script")
	}
	bin := filepath.Join(t.TempDir(), "sandbox")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nsleep 30 &\nwait\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	r := NewCloudRun()
	r.Bin = bin
	r.probeTimeout = 200 * time.Millisecond

	done := make(chan bool, 1)
	start := time.Now()
	go func() { done <- r.Available(context.Background()) }()
	select {
	case ok := <-done:
		if ok {
			t.Fatal("Available = true for a CLI that never answered")
		}
		if d := time.Since(start); d > r.probeTimeout+killGrace+2*time.Second {
			t.Fatalf("probe took %v", d)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Available still blocked 5s after a 200ms probe timeout")
	}
}

// fakeProbeSandbox answers the probe the way the real CLI would: it prints the file
// the probe wrote into the mounted directory. It ignores the interpreter, as
// fakeSandbox does.
func fakeProbeSandbox(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "sandbox")
	script := `#!/bin/sh
src=
while [ $# -gt 0 ]; do
  case "$1" in
    --mount) src=${2#*source=}; src=${src%%,*}; shift 2 ;;
    *) shift ;;
  esac
done
cat "$src/` + probeFile + `" && echo
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin
}

// The probe goes through Run, so it exercises the mount and the run directory's
// permissions, not just the binary.
func TestAvailableReadsTheMountedProbeFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake sandbox is a POSIX shell script")
	}
	r := NewCloudRun()
	r.Bin = fakeProbeSandbox(t)
	if !r.Available(context.Background()) {
		t.Fatal("Available = false although the CLI read the probe file")
	}

	// A CLI that runs but cannot see the mount is not available.
	blind := filepath.Join(t.TempDir(), "sandbox")
	if err := os.WriteFile(blind, []byte("#!/bin/sh\necho 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	r2 := NewCloudRun()
	r2.Bin = blind
	if r2.Available(context.Background()) {
		t.Fatal("Available = true for a CLI that never read the probe file")
	}
}

func TestAvailableSurvivesCancelledFirstContext(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake sandbox is a POSIX shell script")
	}
	r := NewCloudRun()
	r.Bin = fakeProbeSandbox(t)

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if !r.Available(cancelled) {
		t.Fatal("Available = false: a cancelled first context poisoned the cached probe")
	}
	if !r.Available(context.Background()) {
		t.Fatal("cached result changed")
	}
}

func tailOf(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}
