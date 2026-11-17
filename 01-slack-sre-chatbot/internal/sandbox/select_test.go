package sandbox

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// noCLI is a Cloud Run CLI path that never exists, so "auto" stays hermetic.
const noCLI = "/nonexistent/sandbox"

func TestSelect(t *testing.T) {
	// Hermetic: neither a real Cloud Run CLI nor a real Docker daemon may decide "auto".
	t.Setenv("DOCKER_HOST", "unix:///nonexistent.sock")

	for mode, want := range map[string]string{"docker": "docker", "cloudrun": "cloudrun"} {
		r := Select(mode, "sre-agent:local", t.TempDir(), noCLI)
		if r == nil || r.Name() != want {
			t.Errorf("Select(%q) = %v, want %s", mode, r, want)
		}
	}
	if r := Select("auto", "sre-agent:local", t.TempDir(), noCLI); r != nil {
		t.Errorf("Select(auto) with no CloudRun binary and no Docker = %s, want nil", r.Name())
	}
	if r := Select("podman", "sre-agent:local", t.TempDir(), noCLI); r != nil {
		t.Errorf("Select(podman) = %s, want nil", r.Name())
	}

	t.Run("auto prefers Cloud Run when its binary exists", func(t *testing.T) {
		bin := filepath.Join(t.TempDir(), "sandbox")
		if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		r := Select("auto", "sre-agent:local", t.TempDir(), bin)
		if r == nil || r.Name() != "cloudrun" {
			t.Fatalf("Select(auto) = %v, want cloudrun", r)
		}
		if c := r.(*CloudRun); c.Bin != bin {
			t.Errorf("Bin = %q, want %q", c.Bin, bin)
		}
	})

	t.Run("auto falls back to an available Docker", func(t *testing.T) {
		workDir := t.TempDir()
		f := &fakeEngine{onCreate: func(t *testing.T, body createBody) []byte {
			src := strings.TrimSuffix(body.HostConfig.Binds[0], ":/data:ro")
			got, err := os.ReadFile(filepath.Join(src, probeFile))
			if err != nil {
				t.Errorf("read probe file: %v", err)
			}
			return frame(1, string(got)+"\n")
		}}
		t.Setenv("DOCKER_HOST", "unix://"+f.start(t))
		r := Select("auto", "sre-agent:local", workDir, noCLI)
		if r == nil || r.Name() != "docker" {
			t.Fatalf("Select(auto) = %v, want docker", r)
		}
		if d := r.(*Docker); d.ProbeDir != workDir || d.Image != "sre-agent:local" {
			t.Errorf("Docker = %+v, want ProbeDir %q", d, workDir)
		}
	})
}

func TestSelectShim(t *testing.T) {
	r := Select("shim", "", t.TempDir(), "/x/shim")
	if r == nil || r.Name() != "shim" {
		t.Fatalf("Select(shim) = %v, want shim", r)
	}
	if c := r.(*CloudRun); c.Bin != "/x/shim" {
		t.Errorf("Bin = %q, want /x/shim", c.Bin)
	}
	if r := Select("cloudrun", "", t.TempDir(), "/x/sb"); r == nil || r.Name() != "cloudrun" || r.(*CloudRun).Bin != "/x/sb" {
		t.Errorf("Select(cloudrun) = %v, want cloudrun at /x/sb", r)
	}
}

// The shim gives no isolation, so it must only ever be chosen by name.
func TestSelectAutoNeverPicksShim(t *testing.T) {
	t.Setenv("DOCKER_HOST", "unix:///nonexistent.sock")
	if r := Select("auto", "sre-agent:local", t.TempDir(), noCLI); r != nil {
		t.Errorf("Select(auto) = %s, want nil", r.Name())
	}
}

func TestShimUnavailableWhenCLIMissing(t *testing.T) {
	if NewShim("/nonexistent/shim").Available(context.Background()) {
		t.Error("Available() = true for a missing shim")
	}
}
