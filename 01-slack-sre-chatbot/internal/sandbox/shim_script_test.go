//go:build !windows

package sandbox

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// shimScript is the real deploy/local/sandbox-shim, so these tests cover what Compose
// and up.sh actually mount, not a copy.
func shimScript(t *testing.T) string {
	t.Helper()
	p, err := filepath.Abs(filepath.Join("..", "..", "deploy", "local", "sandbox-shim"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("shim script: %v", err)
	}
	return p
}

func runShim(t *testing.T, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	cmd := exec.Command("/bin/sh", append([]string{shimScript(t)}, args...)...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	var ee *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &ee):
		code = ee.ExitCode()
	default:
		t.Fatalf("run shim: %v", err)
	}
	return out.String(), errb.String(), code
}

func TestShimRewritesDataAndExecs(t *testing.T) {
	// A space in the source path: the rewrite must keep /data/<file> one argument.
	dir := filepath.Join(t.TempDir(), "run 1")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "code.sh"), []byte(`echo "$0"`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, errOut, code := runShim(t, "do", "--mount", "type=bind,source="+dir+",destination=/data,readonly",
		"--", "/bin/sh", "/data/code.sh")
	if code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errOut)
	}
	if want := filepath.Join(dir, "code.sh") + "\n"; out != want {
		t.Errorf("stdout = %q, want %q", out, want)
	}
}

// Code reads its evidence from /data, as the skills tell the model to. Without a real
// mount the shim has to point those paths at the run directory inside the code too,
// and leave paths that merely contain "/data" alone.
func TestShimRewritesDataInsideTheCode(t *testing.T) {
	// No space here: the rewrite is textual, so an unquoted /data in shell code would
	// split on a space in the run directory. Run directories never contain one (and
	// /data never does), so that case is documented in the shim, not tested.
	dir := filepath.Join(t.TempDir(), "run-2")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "facts.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	code := `cat "/data/facts.json"` + "\n" +
		`echo '/var/data/keep'` + "\n" +
		`ls /data > /dev/null && echo dir-ok` + "\n"
	if err := os.WriteFile(filepath.Join(dir, "code.sh"), []byte(code), 0o644); err != nil {
		t.Fatal(err)
	}
	out, errOut, exit := runShim(t, "do", "--mount", "type=bind,source="+dir+",destination=/data,readonly",
		"--", "/bin/sh", "/data/code.sh")
	if exit != 0 {
		t.Fatalf("exit %d, stderr %q", exit, errOut)
	}
	if want := "{}/var/data/keep\ndir-ok\n"; out != want {
		t.Errorf("stdout = %q, want %q", out, want)
	}
}

// Misses must not be silent: /data next to shell punctuation, and two /data paths in a
// row, are rewritten too.
func TestShimRewritesDataNextToShellPunctuation(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "run-3")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "facts.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	code := "cd /data;ls\n" +
		"ls /data /data/facts.json\n" +
		"cat /data/facts.json|wc -c|tr -d ' '\n" +
		"echo `cat /data/facts.json`\n"
	if err := os.WriteFile(filepath.Join(dir, "code.sh"), []byte(code), 0o644); err != nil {
		t.Fatal(err)
	}
	out, errOut, exit := runShim(t, "do", "--mount", "type=bind,source="+dir+",destination=/data,readonly",
		"--", "/bin/sh", "/data/code.sh")
	// A missed /data shows up as "No such file or directory" on stderr: there is no
	// /data on the host. An empty stderr means every one of them was rewritten.
	if exit != 0 || errOut != "" {
		t.Fatalf("exit %d, stderr %q", exit, errOut)
	}
	for _, line := range []string{filepath.Join(dir, "facts.json"), "2", "{}"} {
		if !containsLine(out, line) {
			t.Errorf("stdout %q has no line %q", out, line)
		}
	}
}

// The shim starts code in the run directory, not the agent's working directory, so a
// relative path such as .env.local does not reach the agent's files.
func TestShimRunsInTheRunDirectory(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "code.sh"), []byte("pwd -P\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, errOut, exit := runShim(t, "do", "--mount", "type=bind,source="+dir+",destination=/data,readonly",
		"--", "/bin/sh", "/data/code.sh")
	if exit != 0 {
		t.Fatalf("exit %d, stderr %q", exit, errOut)
	}
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	if out != real+"\n" {
		t.Errorf("pwd = %q, want %q", out, real+"\n")
	}
}

// Wherever SANDBOX_CLI points at the shim, the runner must be labelled shim, and "auto"
// must not choose it: doctor would otherwise say cloudrun while code runs unisolated.
func TestSelectRecognisesTheShimByContent(t *testing.T) {
	t.Setenv("DOCKER_HOST", "unix:///nonexistent.sock")
	shim := shimScript(t)
	if r := Select("cloudrun", "", t.TempDir(), shim); r == nil || r.Name() != "shim" {
		t.Errorf("Select(cloudrun, shim path) = %v, want a runner named shim", r)
	}
	if r := Select("auto", "", t.TempDir(), shim); r != nil {
		t.Errorf("Select(auto, shim path) = %s, want nil", r.Name())
	}
	if !IsShim(shim) {
		t.Error("IsShim(shim script) = false")
	}
	bin := filepath.Join(t.TempDir(), "sandbox")
	if err := os.WriteFile(bin, []byte("\x7fELF not a shim"), 0o755); err != nil {
		t.Fatal(err)
	}
	if IsShim(bin) {
		t.Error("IsShim(binary) = true")
	}
}

func containsLine(s, line string) bool {
	return slices.Contains(strings.Split(s, "\n"), line)
}

func TestShimInterpreterFallback(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "code.sh"), []byte("echo fallback\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// /nonexistent/sh is not executable, so the shim must find sh on PATH, the way it
	// finds /bin/bash on macOS when it is asked for /usr/bin/bash.
	out, errOut, code := runShim(t, "do", "--mount", "type=bind,source="+dir+",destination=/data,readonly",
		"--", "/nonexistent/sh", "/data/code.sh")
	if code != 0 || out != "fallback\n" {
		t.Errorf("exit %d, stdout %q, stderr %q", code, out, errOut)
	}
}

func TestShimUsageErrors(t *testing.T) {
	for name, args := range map[string][]string{
		"no args":          nil,
		"not do":           {"run", "--", "/bin/sh"},
		"no separator":     {"do", "/bin/sh"},
		"nothing after --": {"do", "--"},
	} {
		if _, _, code := runShim(t, args...); code != 2 {
			t.Errorf("%s: exit %d, want 2", name, code)
		}
	}
}

// The whole point of the shim: production's runner code, unchanged, driving it.
func TestShimThroughCloudRunRunner(t *testing.T) {
	ctx := context.Background()
	s := NewShim(shimScript(t))
	res, err := s.Run(ctx, "bash", "echo hi", t.TempDir())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.ExitCode != 0 || res.Stdout != "hi\n" {
		t.Errorf("Run = %+v, want exit 0 and stdout %q", res, "hi\n")
	}

	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not on PATH; the probe runs python")
	}
	if !NewShim(shimScript(t)).Available(ctx) {
		t.Error("Available() = false through the shim")
	}
}
