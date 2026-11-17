package sandbox

import (
	"bytes"
	"context"
	"io"
	"os"
)

// Select returns the code runner for SANDBOX: "cloudrun", "docker" or "shim" as
// asked, and for "auto" Cloud Run when its CLI is installed, else Docker when its
// probe passes, else nil. "auto" never picks the shim: it runs model code with no
// isolation, so it has to be asked for by name.
//
// cli is the Cloud Run CLI path (SANDBOX_CLI), or the shim's path in "shim" mode; ""
// means the standard install location. The Docker probe is cached and time-bounded,
// so "auto" cannot hang startup. workDir must be the directory runs use, so the
// probe exercises the same bind source.
func Select(mode, image, workDir, cli string) Runner {
	if cli == "" {
		cli = cloudRunBin
	}
	switch mode {
	case "cloudrun":
		if IsShim(cli) {
			return NewShim(cli)
		}
		return cloudRunAt(cli)
	case "shim":
		return NewShim(cli)
	case "docker":
		return NewDocker(image, workDir)
	case "auto":
		if fileExists(cli) && !IsShim(cli) {
			return cloudRunAt(cli)
		}
		if d := NewDocker(image, workDir); d.Available(context.Background()) {
			return d
		}
	}
	return nil
}

func cloudRunAt(cli string) *CloudRun {
	c := NewCloudRun()
	c.Bin = cli
	return c
}

// shimMarker is a line in deploy/local/sandbox-shim's header. Looking for it, rather
// than trusting SANDBOX, is what keeps the shim from ever running under the cloudrun
// label: compose mounts it at the real CLI's path, and .env.local points SANDBOX_CLI
// at it, so a SANDBOX=cloudrun or auto would otherwise drive it unannounced.
const shimMarker = "sre-agent-sandbox-shim"

// IsShim reports whether path is the local shim rather than the Cloud Run CLI. The real
// CLI is a binary; the shim is a short script whose header names itself.
func IsShim(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	head := make([]byte, 1024)
	n, _ := io.ReadFull(f, head)
	return bytes.Contains(head[:n], []byte(shimMarker))
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
