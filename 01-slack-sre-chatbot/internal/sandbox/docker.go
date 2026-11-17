package sandbox

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Docker runs code in a throwaway container through the Docker Engine API on a
// Unix socket, for local stacks without Cloud Run. It uses the API directly, not
// the docker CLI, so the agent image needs nothing extra and go run behaves the
// same as the Compose container.
//
// As with commandArgs for CloudRun, the isolation is entirely createRequest's
// body: it must never gain a network, an env var, a writable or second mount, a
// capability, a privileged flag, or a root user (see sandboxUser).
type Docker struct {
	Socket    string        // default from DOCKER_HOST (unix:// only) else "/var/run/docker.sock"
	Image     string        // SANDBOX_IMAGE
	ProbeDir  string        // the agent's WorkDir; the probe must use the same directory runs use
	Timeout   time.Duration // default 60s
	MaxOutput int           // default 16384

	once     sync.Once
	avail    bool
	probeErr error // why the probe failed; set with avail
	// probeTimeout bounds the startup probe; 0 means defaultProbeTimeout. Tests
	// shorten it.
	probeTimeout time.Duration

	clientOnce sync.Once
	client     *http.Client

	apiMu sync.Mutex
	api   string // the negotiated "/vX.Y" path prefix; see apiPrefix
}

// The Engine API version is negotiated, not pinned: a daemon rejects a client
// older than its own minimum (1.44 on Engine 29.0 to 29.2), so any fixed version
// eventually breaks on new daemons or old ones. Calls use the daemon's version
// capped at maxDockerAPI, the newest this code was written against. Below
// minDockerAPI (Engine 25) the runner is unavailable rather than trusting a
// daemon that might ignore fields of createRequest it does not know.
var (
	minDockerAPI = apiVersion{1, 44}
	maxDockerAPI = apiVersion{1, 47}
)

type apiVersion struct{ major, minor int }

// parseAPIVersion reads "major.minor", so 1.5 is older than 1.44, not newer.
func parseAPIVersion(s string) (apiVersion, bool) {
	maj, minr, ok := strings.Cut(s, ".")
	if !ok {
		return apiVersion{}, false
	}
	a, err1 := strconv.Atoi(maj)
	b, err2 := strconv.Atoi(minr)
	if err1 != nil || err2 != nil || a < 0 || b < 0 {
		return apiVersion{}, false
	}
	return apiVersion{a, b}, true
}

func (v apiVersion) less(w apiVersion) bool {
	return v.major < w.major || v.major == w.major && v.minor < w.minor
}

func (v apiVersion) String() string { return fmt.Sprintf("%d.%d", v.major, v.minor) }

const defaultDockerSocket = "/var/run/docker.sock"

// cleanupTimeout bounds the kill and, separately, the removal, on a context
// detached from the run's: a timed-out or cancelled run must still remove its
// container, and a wedged daemon must not hold the caller for long.
const cleanupTimeout = 5 * time.Second

// logsTimeout bounds reading an exited container's output, detached from the
// run's deadline and the caller's cancellation.
const logsTimeout = 10 * time.Second

// maxAPIBody bounds a JSON reply read from the daemon.
const maxAPIBody = 1 << 20

func NewDocker(image, probeDir string) *Docker {
	sock := defaultDockerSocket
	if s, ok := strings.CutPrefix(os.Getenv("DOCKER_HOST"), "unix://"); ok && s != "" {
		sock = s
	}
	return &Docker{
		Socket:    sock,
		Image:     image,
		ProbeDir:  probeDir,
		Timeout:   60 * time.Second,
		MaxOutput: 16384,
	}
}

func (d *Docker) Name() string { return "docker" }

// Available reports whether the daemon answers with a supported API version, the
// image is present, and a container can read a file from its bind mount. Probed
// once and cached, with the caller's cancellation detached, for the same reasons
// as CloudRun.Available.
func (d *Docker) Available(ctx context.Context) bool {
	d.once.Do(func() {
		d.probeErr = d.probe(context.WithoutCancel(ctx))
		d.avail = d.probeErr == nil
	})
	return d.avail
}

// ProbeError is why Available returned false: nil once it has returned true, or
// before it has run. Call it after Available.
func (d *Docker) ProbeError() error { return d.probeErr }

// probe goes through Run over a fresh directory under ProbeDir, made the way a
// run's is. The daemon resolves bind sources on its own host, so a WorkDir that
// is not the same path there (a Compose volume at a different path, a folder
// Docker Desktop does not share) mounts an empty /data: the probe turns that
// into "unavailable" instead of every run_code call failing. Bounded by the
// probe timeout plus Run's cleanup. Every failure says why, for the startup log.
func (d *Docker) probe(ctx context.Context) error {
	if d.Image == "" {
		return errors.New("no SANDBOX_IMAGE")
	}
	timeout := d.probeTimeout
	if timeout == 0 {
		timeout = defaultProbeTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if _, err := d.apiPrefix(ctx); err != nil {
		return fmt.Errorf("docker at %s: %w", d.Socket, err)
	}
	if err := d.do(ctx, http.MethodGet, "/images/"+d.Image+"/json", nil, nil); err != nil {
		return fmt.Errorf("image %s: %w", d.Image, err)
	}
	base := d.ProbeDir
	if base == "" {
		base = filepath.Join(os.TempDir(), "sre-runs") // agentloop's default WorkDir
	}
	if err := os.MkdirAll(base, 0o755); err != nil {
		return err
	}
	dir, err := os.MkdirTemp(base, "probe-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	if err := os.WriteFile(filepath.Join(dir, probeFile), []byte(probeValue), 0o644); err != nil {
		return err
	}
	res, err := d.Run(ctx, "python", "print(open('/data/"+probeFile+"').read())", dir)
	switch {
	case err != nil:
		return fmt.Errorf("probe container: %w", err)
	case res.TimedOut:
		return errors.New("probe container timed out")
	case res.ExitCode != 0:
		return fmt.Errorf("probe container exited %d: %s", res.ExitCode, strings.TrimSpace(res.Stderr))
	case res.Stdout != probeValue+"\n":
		return fmt.Errorf("probe container did not read %s through its bind mount of %s; is that folder shared with Docker?", probeFile, dir)
	}
	return nil
}

// Run creates, starts, waits for and reads a container, and removes it on every
// path once created. The container is named before it exists, so a create whose
// reply never arrives can still be removed.
func (d *Docker) Run(ctx context.Context, lang, code, dataDir string) (Result, error) {
	interp, err := interpreterFor(lang)
	if err != nil {
		return Result{}, err
	}
	// The daemon needs an absolute bind source, and the bind spec is
	// colon-separated: a colon in the path would shift the destination and mode.
	dataDir, err = filepath.Abs(dataDir)
	if err != nil {
		return Result{}, err
	}
	if strings.Contains(dataDir, ":") {
		return Result{}, fmt.Errorf("data dir %q contains a colon", dataDir)
	}
	file, err := writeCodeFile(dataDir, lang, code)
	if err != nil {
		return Result{}, err
	}
	defer os.Remove(filepath.Join(dataDir, file))

	runCtx, cancel := context.WithTimeout(ctx, d.Timeout)
	defer cancel()

	name, err := containerName()
	if err != nil {
		return Result{}, err
	}
	var created struct{ Id string }
	err = d.do(runCtx, http.MethodPost, "/containers/create?name="+name, createRequest(d.Image, dataDir, interp, file), &created)
	if err != nil {
		var apiErr *apiError
		if !errors.As(err, &apiErr) {
			d.remove(ctx, name) // the daemon may have created it before the reply was lost
		}
		return runFailed(ctx, runCtx, "create container", err)
	}
	id := created.Id
	if id == "" {
		id = name
	}
	defer d.remove(ctx, id)

	if err := d.do(runCtx, http.MethodPost, "/containers/"+id+"/start", nil, nil); err != nil {
		return runFailed(ctx, runCtx, "start container", err)
	}

	var waited struct {
		StatusCode int
		Error      *struct{ Message string }
	}
	if err := d.do(runCtx, http.MethodPost, "/containers/"+id+"/wait", nil, &waited); err != nil {
		if ctx.Err() == nil && runCtx.Err() != nil {
			// Timed out: stop it now, keep what it printed. Removal is deferred.
			cctx, ccancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
			defer ccancel()
			_ = d.do(cctx, http.MethodPost, "/containers/"+id+"/kill", nil, nil)
			res := Result{TimedOut: true, ExitCode: -1}
			res.Stdout, res.Stderr, _ = d.logs(cctx, id)
			return res, nil
		}
		return runFailed(ctx, runCtx, "wait for container", err)
	}
	if waited.Error != nil && waited.Error.Message != "" {
		return Result{}, fmt.Errorf("docker wait for container: %s", waited.Error.Message)
	}

	// The script has finished, so the run's deadline no longer applies: a script
	// that exits near it, or printed a lot, keeps its real exit code.
	lctx, lcancel := context.WithTimeout(context.WithoutCancel(ctx), logsTimeout)
	defer lcancel()
	res := Result{ExitCode: waited.StatusCode}
	res.Stdout, res.Stderr, err = d.logs(lctx, id)
	if err != nil {
		return res, fmt.Errorf("docker read container logs: %w", err)
	}
	return res, nil
}

// runFailed maps a failed API call to Run's result: the caller's cancellation
// wins, then the run's own timeout, then the error itself.
func runFailed(ctx, runCtx context.Context, what string, err error) (Result, error) {
	if ctx.Err() != nil {
		return Result{}, ctx.Err()
	}
	if runCtx.Err() != nil {
		return Result{TimedOut: true, ExitCode: -1}, nil
	}
	return Result{}, fmt.Errorf("docker %s: %w", what, err)
}

// createRequest is the body of POST /containers/create, and the whole isolation
// policy; see the Docker type.
func createRequest(image, dataDir, interpreter, file string) map[string]any {
	return map[string]any{
		"Image": image,
		// The entrypoint, not Cmd: the default image's ENTRYPOINT is /agent, and
		// Docker appends Cmd to it. Cmd stays empty so the image's CMD is not
		// appended either.
		"Entrypoint":      []string{interpreter, "/data/" + file},
		"Cmd":             []string{},
		"Env":             []string{},
		"User":            sandboxUser(os.Getuid(), os.Getgid()),
		"NetworkDisabled": true,
		// The log stream is only multiplexed (and so demuxable) without a TTY.
		"Tty": false,
		"HostConfig": map[string]any{
			"NetworkMode":    "none",
			"ReadonlyRootfs": true,
			"Tmpfs":          map[string]string{"/tmp": ""},
			"CapDrop":        []string{"ALL"},
			"SecurityOpt":    []string{"no-new-privileges"},
			"Memory":         268435456,
			"NanoCPUs":       1000000000,
			"Binds":          []string{dataDir + ":/data:ro"},
		},
	}
}

// sandboxUser is the container's uid:gid: the agent's own, because run
// directories are 0700 and owned by the agent, and only the owner can read them
// through the mount (the developer's uid under go run, 65532 under Compose). It
// is never root: a root agent, or one with no uids (Windows), gets the image's
// 65532:65532, and a root group is replaced by 65532, which the 0700 directory
// does not need.
func sandboxUser(uid, gid int) string {
	const nobody = 65532
	if uid <= 0 {
		uid, gid = nobody, nobody
	}
	if gid <= 0 {
		gid = nobody
	}
	return fmt.Sprintf("%d:%d", uid, gid)
}

// containerName is random, not pid-based: the agent in Compose and a go run on
// the host share one daemon.
func containerName() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "sre-sandbox-" + hex.EncodeToString(b[:]), nil
}

// remove force-removes the container (killing it if still running) on a fresh
// budget, so it still happens after the run's context has expired.
func (d *Docker) remove(ctx context.Context, id string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
	defer cancel()
	_ = d.do(ctx, http.MethodDelete, "/containers/"+id+"?force=1", nil, nil)
}

// logs reads the container's whole output into capped buffers, so a script that
// printed in a loop costs bounded memory. It returns what it got even on error.
func (d *Docker) logs(ctx context.Context, id string) (stdout, stderr string, err error) {
	// Tracebacks sit at the end of stderr, so it keeps its tail; stdout keeps its head.
	out := &capBuffer{max: d.MaxOutput}
	errOut := &capBuffer{max: d.MaxOutput, keepTail: true}
	resp, err := d.call(ctx, http.MethodGet, "/containers/"+id+"/logs?stdout=1&stderr=1", nil)
	if err == nil {
		err = demux(resp.Body, out, errOut)
		resp.Body.Close()
	}
	return out.String(), errOut.String(), err
}

// demux splits the Engine's multiplexed log stream for a non-TTY container. Each
// frame is an 8-byte header (stream type, three zero bytes, big-endian uint32
// length) followed by that many bytes.
func demux(r io.Reader, stdout, stderr io.Writer) error {
	var hdr [8]byte
	for {
		if _, err := io.ReadFull(r, hdr[:]); err != nil {
			if err == io.EOF {
				return nil
			}
			return fmt.Errorf("log stream header: %w", err)
		}
		var w io.Writer
		switch hdr[0] {
		case 0, 1: // stdin is never attached; the Engine's own client writes it to stdout
			w = stdout
		case 2:
			w = stderr
		default:
			return fmt.Errorf("log stream: unknown stream type %d", hdr[0])
		}
		if _, err := io.CopyN(w, r, int64(binary.BigEndian.Uint32(hdr[4:]))); err != nil {
			return fmt.Errorf("log stream frame: %w", err)
		}
	}
}

// apiError is a reply from the daemon with a failure status, as opposed to no
// reply at all.
type apiError struct {
	status int
	msg    string
}

func (e *apiError) Error() string { return fmt.Sprintf("status %d: %s", e.status, e.msg) }

func (d *Docker) httpClient() *http.Client {
	d.clientOnce.Do(func() {
		d.client = &http.Client{Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var dialer net.Dialer
				return dialer.DialContext(ctx, "unix", d.Socket)
			},
			IdleConnTimeout: 30 * time.Second,
		}}
	})
	return d.client
}

// apiPrefix returns the "/vX.Y" path prefix, negotiating it on first use from an
// unversioned GET /_ping, which a daemon of any version answers with its own
// version in Api-Version. Success is cached; a failure is returned and the next
// call tries again.
func (d *Docker) apiPrefix(ctx context.Context) (string, error) {
	d.apiMu.Lock()
	defer d.apiMu.Unlock()
	if d.api != "" {
		return d.api, nil
	}
	resp, err := d.send(ctx, http.MethodGet, "/_ping", nil)
	if err != nil {
		return "", err
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxAPIBody))
	resp.Body.Close()
	h := resp.Header.Get("Api-Version")
	if h == "" {
		return "", errors.New("the daemon sent no Api-Version header on /_ping")
	}
	v, ok := parseAPIVersion(h)
	switch {
	case !ok:
		return "", fmt.Errorf("the daemon sent an unparsable Api-Version %q", h)
	case v.less(minDockerAPI):
		return "", fmt.Errorf("the daemon's API version %s is older than %s, the oldest this runner supports (Docker Engine 25)", v, minDockerAPI)
	case maxDockerAPI.less(v):
		v = maxDockerAPI
	}
	d.api = "/v" + v.String()
	return d.api, nil
}

// call sends one Engine API request at the negotiated version.
func (d *Docker) call(ctx context.Context, method, path string, body any) (*http.Response, error) {
	api, err := d.apiPrefix(ctx)
	if err != nil {
		return nil, err
	}
	return d.send(ctx, method, api+path, body)
}

// send sends one request to the daemon as is; every call is bounded by ctx alone.
// A 304 (start of an already started container, kill of a stopped one) is success.
func (d *Docker) send(ctx context.Context, method, path string, body any) (*http.Response, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://docker"+path, rd)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := d.httpClient().Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 && resp.StatusCode != http.StatusNotModified {
		defer resp.Body.Close()
		var e struct{ Message string }
		_ = json.NewDecoder(io.LimitReader(resp.Body, maxAPIBody)).Decode(&e)
		return nil, &apiError{status: resp.StatusCode, msg: method + " " + path + ": " + e.Message}
	}
	return resp, nil
}

// do is call for JSON replies; out nil discards the body.
func (d *Docker) do(ctx context.Context, method, path string, body, out any) error {
	resp, err := d.call(ctx, method, path, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	r := io.LimitReader(resp.Body, maxAPIBody)
	if out != nil {
		return json.NewDecoder(r).Decode(out)
	}
	_, err = io.Copy(io.Discard, r)
	return err
}
