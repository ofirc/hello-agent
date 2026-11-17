//go:build windows

package sandbox

import "os/exec"

// Windows has no process groups; the sandbox CLI is Linux-only anyway, so
// this only has to compile.
func setProcessGroup(*exec.Cmd) {}

func killProcessGroup(cmd *exec.Cmd) error { return cmd.Process.Kill() }
