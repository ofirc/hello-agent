package sandbox

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestCreateRequestHardening(t *testing.T) {
	raw, err := json.Marshal(createRequest("sre-agent:local", "/x/run-1", "/usr/bin/python3", ".code-1.py"))
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Image           string
		Entrypoint      []string
		Cmd             []string
		Env             []string
		User            string
		NetworkDisabled bool
		Tty             bool
		HostConfig      struct {
			NetworkMode    string
			ReadonlyRootfs bool
			Tmpfs          map[string]string
			CapDrop        []string
			SecurityOpt    []string
			Memory         int64
			NanoCPUs       int64
			Binds          []string
		}
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	h := got.HostConfig
	switch {
	case got.Image != "sre-agent:local":
		t.Errorf("Image = %q", got.Image)
	case got.Env == nil || len(got.Env) != 0:
		t.Errorf("Env = %#v, want an empty list", got.Env)
	case !got.NetworkDisabled || h.NetworkMode != "none":
		t.Errorf("network not disabled: NetworkDisabled %v, NetworkMode %q", got.NetworkDisabled, h.NetworkMode)
	case got.Tty:
		t.Error("Tty = true; the log stream must stay multiplexed")
	case !h.ReadonlyRootfs || len(h.Tmpfs) != 1 || h.Tmpfs["/tmp"] != "":
		t.Errorf("rootfs: ReadonlyRootfs %v, Tmpfs %v", h.ReadonlyRootfs, h.Tmpfs)
	case !slices.Equal(h.CapDrop, []string{"ALL"}):
		t.Errorf("CapDrop = %q", h.CapDrop)
	case !slices.Equal(h.SecurityOpt, []string{"no-new-privileges"}):
		t.Errorf("SecurityOpt = %q", h.SecurityOpt)
	case got.User != sandboxUser(os.Getuid(), os.Getgid()):
		t.Errorf("User = %q, want the agent's own %q", got.User, sandboxUser(os.Getuid(), os.Getgid()))
	case got.User == "" || strings.HasPrefix(got.User, "0:") || strings.HasSuffix(got.User, ":0") ||
		strings.HasPrefix(got.User, "root"):
		t.Errorf("User = %q runs as root", got.User)
	case h.Memory != 268435456 || h.NanoCPUs != 1000000000:
		t.Errorf("limits: Memory %d, NanoCPUs %d", h.Memory, h.NanoCPUs)
	case !slices.Equal(h.Binds, []string{"/x/run-1:/data:ro"}):
		t.Errorf("Binds = %q, want exactly one read-only bind", h.Binds)
	// The image's own ENTRYPOINT is /agent: a Cmd alone would run as its arguments.
	case !slices.Equal(got.Entrypoint, []string{"/usr/bin/python3", "/data/.code-1.py"}):
		t.Errorf("Entrypoint = %q", got.Entrypoint)
	case got.Cmd == nil || len(got.Cmd) != 0:
		t.Errorf("Cmd = %#v, want an empty list", got.Cmd)
	}

	// Anything else in the body (Privileged, CapAdd, Devices, Mounts, PortBindings...)
	// would relax the isolation, so the key sets are fixed.
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		t.Fatal(err)
	}
	var host map[string]json.RawMessage
	if err := json.Unmarshal(top["HostConfig"], &host); err != nil {
		t.Fatal(err)
	}
	wantTop := []string{"Cmd", "Entrypoint", "Env", "HostConfig", "Image", "NetworkDisabled", "Tty", "User"}
	wantHost := []string{"Binds", "CapDrop", "Memory", "NanoCPUs", "NetworkMode", "ReadonlyRootfs", "SecurityOpt", "Tmpfs"}
	if k := sortedKeys(top); !slices.Equal(k, wantTop) {
		t.Errorf("body keys = %q, want %q", k, wantTop)
	}
	if k := sortedKeys(host); !slices.Equal(k, wantHost) {
		t.Errorf("HostConfig keys = %q, want %q", k, wantHost)
	}
}

func TestSandboxUserNeverRoot(t *testing.T) {
	for _, tc := range []struct {
		uid, gid int
		want     string
	}{
		{501, 20, "501:20"},           // go run on a Mac: the developer, who owns the 0700 run dir
		{65532, 65532, "65532:65532"}, // the agent image's user under Compose
		{0, 0, "65532:65532"},         // agent as root: never pass root on
		{0, 20, "65532:65532"},
		{501, 0, "501:65532"},   // root group: keep the owner, drop the group
		{-1, -1, "65532:65532"}, // no uids (Windows)
	} {
		if got := sandboxUser(tc.uid, tc.gid); got != tc.want {
			t.Errorf("sandboxUser(%d, %d) = %q, want %q", tc.uid, tc.gid, got, tc.want)
		}
	}
}

func sortedKeys(m map[string]json.RawMessage) []string {
	var k []string
	for key := range m {
		k = append(k, key)
	}
	slices.Sort(k)
	return k
}

// fakeEngine is a minimal Docker Engine API on a Unix socket. It serves one
// container, "c1", and records every call as "METHOD /path?query" without the
// version prefix. Like a real daemon it answers an unversioned /_ping with its
// version in Api-Version; every other call must carry wantAPI.
type fakeEngine struct {
	apiVersion   string // "" means "1.54", this Mac's daemon
	noAPIVersion bool   // /_ping sends no Api-Version header
	wantAPI      string // "" means "/v1.47"
	exitCode     int
	logs         []byte // multiplexed frames
	blockWait    bool
	waitDelay    time.Duration // before wait answers
	logsDelay    time.Duration // before logs answers
	failStart    bool
	missingImage bool
	// onCreate sees the create body and the container's command; when set,
	// its return value replaces logs. It runs while the code file still exists.
	onCreate func(t *testing.T, body createBody) []byte

	mu      sync.Mutex
	calls   []string
	created []createBody
}

type createBody struct {
	Entrypoint []string
	Cmd        []string
	HostConfig struct{ Binds []string }
}

// runsInterpreter is how a real sre-agent:local container behaves: its image
// ENTRYPOINT is /agent, so unless the body replaces the entrypoint with an
// interpreter (and empties Cmd), the script never runs.
func (b createBody) runsInterpreter() bool {
	return len(b.Entrypoint) == 2 && (b.Entrypoint[0] == "/usr/bin/python3" || b.Entrypoint[0] == "/usr/bin/bash") &&
		strings.HasPrefix(b.Entrypoint[1], "/data/") && b.Cmd != nil && len(b.Cmd) == 0
}

func (f *fakeEngine) api() string {
	if f.wantAPI == "" {
		return "/v1.47"
	}
	return f.wantAPI
}

func (f *fakeEngine) record(r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	call := r.Method + " " + strings.TrimPrefix(r.URL.Path, f.api())
	if r.URL.RawQuery != "" {
		call += "?" + r.URL.RawQuery
	}
	f.calls = append(f.calls, call)
}

func (f *fakeEngine) callList() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

func (f *fakeEngine) called(prefix string) bool {
	for _, c := range f.callList() {
		if strings.HasPrefix(c, prefix) {
			return true
		}
	}
	return false
}

// start serves f on a fresh socket and returns its path. The socket lives in a
// short os.MkdirTemp directory: macOS caps Unix socket paths near 104 bytes, and
// t.TempDir paths can exceed it.
func (f *fakeEngine) start(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "dk")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "d.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	release := make(chan struct{})
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.serve(t, w, r, release)
	}))
	srv.Listener.Close()
	srv.Listener = l
	srv.Start()
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) }) // runs first: unblock a stuck wait before Close
	return sock
}

func (f *fakeEngine) serve(t *testing.T, w http.ResponseWriter, r *http.Request, release chan struct{}) {
	f.record(r)
	if r.Method == http.MethodGet && r.URL.Path == "/_ping" {
		if !f.noAPIVersion {
			v := f.apiVersion
			if v == "" {
				v = "1.54"
			}
			w.Header().Set("Api-Version", v)
		}
		w.Write([]byte("OK"))
		return
	}
	rest, ok := strings.CutPrefix(r.URL.Path, f.api()+"/")
	if !ok {
		t.Errorf("%s %s: want API version %s", r.Method, r.URL.Path, f.api())
		http.Error(w, `{"message":"client version mismatch"}`, http.StatusBadRequest)
		return
	}
	path := "/" + rest
	switch {
	case r.Method == http.MethodGet && strings.HasPrefix(path, "/images/") && strings.HasSuffix(path, "/json"):
		if f.missingImage {
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"message":"No such image"}`))
			return
		}
		w.Write([]byte(`{"Id":"sha256:abc"}`))
	case r.Method == http.MethodPost && path == "/containers/create":
		var body createBody
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("create body: %v", err)
		}
		if !body.runsInterpreter() {
			t.Errorf("create body would run the image's /agent entrypoint: Entrypoint %q, Cmd %#v", body.Entrypoint, body.Cmd)
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`{"message":"entrypoint is not an interpreter"}`))
			return
		}
		f.mu.Lock()
		f.created = append(f.created, body)
		onCreate := f.onCreate
		f.mu.Unlock()
		if onCreate != nil {
			logs := onCreate(t, body)
			f.mu.Lock()
			f.logs = logs
			f.mu.Unlock()
		}
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"Id":"c1","Warnings":[]}`))
	case r.Method == http.MethodPost && path == "/containers/c1/start":
		if f.failStart {
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(`{"message":"start failed"}`))
			return
		}
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodPost && path == "/containers/c1/wait":
		if f.blockWait {
			select {
			case <-r.Context().Done():
			case <-release:
			}
			return
		}
		if !sleepCtx(r, f.waitDelay, release) {
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"StatusCode": f.exitCode})
	case r.Method == http.MethodGet && path == "/containers/c1/logs":
		if !sleepCtx(r, f.logsDelay, release) {
			return
		}
		f.mu.Lock()
		logs := f.logs
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/vnd.docker.multiplexed-stream")
		w.Write(logs)
	case r.Method == http.MethodPost && path == "/containers/c1/kill":
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodDelete && strings.HasPrefix(path, "/containers/"):
		w.WriteHeader(http.StatusNoContent)
	default:
		t.Errorf("unexpected call %s %s", r.Method, r.URL)
		w.WriteHeader(http.StatusNotFound)
	}
}

// sleepCtx waits d unless the client goes away or the test ends first.
func sleepCtx(r *http.Request, d time.Duration, release chan struct{}) bool {
	if d == 0 {
		return true
	}
	select {
	case <-time.After(d):
		return true
	case <-r.Context().Done():
	case <-release:
	}
	return false
}

// frame is one multiplexed log frame: stream 1 is stdout, 2 is stderr.
func frame(stream byte, s string) []byte {
	hdr := make([]byte, 8, 8+len(s))
	hdr[0] = stream
	binary.BigEndian.PutUint32(hdr[4:], uint32(len(s)))
	return append(hdr, s...)
}

func frames(fs ...[]byte) []byte {
	var out []byte
	for _, f := range fs {
		out = append(out, f...)
	}
	return out
}

func newTestDocker(t *testing.T, f *fakeEngine) *Docker {
	t.Helper()
	d := NewDocker("sre-agent:local", t.TempDir())
	d.Socket = f.start(t)
	// Generous: only the timeout case may expire.
	d.Timeout = 10 * time.Second
	return d
}

func TestDockerRunSuccess(t *testing.T) {
	const code = "print('hi')"
	f := &fakeEngine{onCreate: func(t *testing.T, body createBody) []byte {
		if len(body.HostConfig.Binds) != 1 {
			t.Errorf("create body = %+v", body)
			return nil
		}
		src := strings.TrimSuffix(body.HostConfig.Binds[0], ":/data:ro")
		got, err := os.ReadFile(filepath.Join(src, strings.TrimPrefix(body.Entrypoint[1], "/data/")))
		if err != nil || string(got) != code {
			t.Errorf("code file under the bind source = %q, %v", got, err)
		}
		return frame(1, "hi\n")
	}}
	d := newTestDocker(t, f)
	dir := t.TempDir()

	res, err := d.Run(context.Background(), "python", code, dir)
	if err != nil || res.Stdout != "hi\n" || res.Stderr != "" || res.ExitCode != 0 || res.TimedOut {
		t.Fatalf("res = %+v, err = %v", res, err)
	}
	calls := f.callList()
	want := []string{
		"GET /_ping", // version negotiation, on the first call
		"POST /containers/create", "POST /containers/c1/start", "POST /containers/c1/wait",
		"GET /containers/c1/logs?stdout=1&stderr=1", "DELETE /containers/c1?force=1",
	}
	if len(calls) != len(want) {
		t.Fatalf("calls = %q, want %q", calls, want)
	}
	for i, c := range calls {
		if !strings.HasPrefix(c, want[i]) {
			t.Fatalf("calls = %q, want %q", calls, want)
		}
	}
	if b := f.created[0].HostConfig.Binds; !slices.Equal(b, []string{dir + ":/data:ro"}) {
		t.Errorf("Binds = %q", b)
	}
	if left, _ := filepath.Glob(filepath.Join(dir, ".code-*")); len(left) != 0 {
		t.Errorf("leftover code files: %v", left)
	}
}

func TestDockerRunNonZero(t *testing.T) {
	f := &fakeEngine{exitCode: 3, logs: frame(2, "oops\n")}
	d := newTestDocker(t, f)
	res, err := d.Run(context.Background(), "bash", "echo oops >&2; exit 3", t.TempDir())
	if err != nil || res.ExitCode != 3 || res.Stderr != "oops\n" || res.TimedOut {
		t.Fatalf("res = %+v, err = %v", res, err)
	}
	if !f.called("DELETE /containers/c1?force=1") {
		t.Errorf("container not removed: %q", f.callList())
	}
}

func TestDockerRunTimeout(t *testing.T) {
	f := &fakeEngine{blockWait: true}
	d := newTestDocker(t, f)
	d.Timeout = 200 * time.Millisecond
	start := time.Now()
	res, err := d.Run(context.Background(), "bash", "sleep 30", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if !res.TimedOut || res.ExitCode != -1 {
		t.Fatalf("res = %+v, want TimedOut", res)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("took %v, want < 2s", d)
	}
	if !f.called("POST /containers/c1/kill") || !f.called("DELETE /containers/c1?force=1") {
		t.Fatalf("want kill and DELETE, calls = %q", f.callList())
	}
}

// A script that finished in time is not a timeout because reading its output ran
// past the run's deadline: wait answered, so its exit code stands.
func TestDockerRunSlowLogsAfterExitIsNotATimeout(t *testing.T) {
	f := &fakeEngine{waitDelay: 600 * time.Millisecond, logsDelay: time.Second, logs: frame(1, "done\n")}
	d := newTestDocker(t, f)
	d.Timeout = time.Second
	res, err := d.Run(context.Background(), "bash", "echo done", t.TempDir())
	if err != nil || res.TimedOut || res.ExitCode != 0 || res.Stdout != "done\n" {
		t.Fatalf("res = %+v, err = %v; want exit 0 with output, not a timeout", res, err)
	}
	if f.called("POST /containers/c1/kill") || !f.called("DELETE /containers/c1?force=1") {
		t.Fatalf("want DELETE and no kill, calls = %q", f.callList())
	}
}

func TestDockerRunTruncation(t *testing.T) {
	big := strings.Repeat("x", 20000)
	f := &fakeEngine{logs: frames(
		frame(1, "HEAD\n"), frame(2, "HEAD\n"),
		frame(1, big), frame(2, big),
		frame(1, "END\n"), frame(2, "END\n"),
	)}
	d := newTestDocker(t, f)
	res, err := d.Run(context.Background(), "bash", "true", t.TempDir())
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
}

// A created container is removed even when it never ran.
func TestDockerRunRemovesContainerWhenStartFails(t *testing.T) {
	f := &fakeEngine{failStart: true}
	d := newTestDocker(t, f)
	if _, err := d.Run(context.Background(), "bash", "true", t.TempDir()); err == nil {
		t.Fatal("want error")
	}
	if !f.called("DELETE /containers/c1?force=1") {
		t.Fatalf("container not removed: %q", f.callList())
	}
	if f.called("POST /containers/c1/wait") {
		t.Fatalf("waited on a container that never started: %q", f.callList())
	}
}

func TestDockerRunRejectsColonInDataDir(t *testing.T) {
	f := &fakeEngine{}
	d := newTestDocker(t, f)
	dir := filepath.Join(t.TempDir(), "a:b")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Run(context.Background(), "bash", "true", dir); err == nil {
		t.Fatal("want error: a colon would shift the bind spec's destination and mode")
	}
	if f.called("POST /containers/create") {
		t.Fatalf("created a container: %q", f.callList())
	}
}

// echoProbe plays a container that reads the probe file through its bind mount,
// which must be under probeDir.
func echoProbe(probeDir string) func(*testing.T, createBody) []byte {
	return func(t *testing.T, body createBody) []byte {
		src := strings.TrimSuffix(body.HostConfig.Binds[0], ":/data:ro")
		if !strings.HasPrefix(src, probeDir+string(filepath.Separator)) {
			t.Errorf("probe bind source %q is not under ProbeDir %q", src, probeDir)
			return nil
		}
		got, err := os.ReadFile(filepath.Join(src, probeFile))
		if err != nil {
			t.Errorf("read probe file: %v", err)
			return nil
		}
		return frame(1, string(got)+"\n")
	}
}

func TestDockerAvailableProbesThroughMount(t *testing.T) {
	probeDir := t.TempDir()
	f := &fakeEngine{onCreate: echoProbe(probeDir)}
	d := NewDocker("sre-agent:local", probeDir)
	d.Socket = f.start(t)

	// The cached result must not depend on the first caller's context.
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if !d.Available(cancelled) {
		t.Fatalf("Available = false although the container read the probe file: %v; calls = %q", d.ProbeError(), f.callList())
	}
	if err := d.ProbeError(); err != nil {
		t.Errorf("ProbeError = %v after a successful probe", err)
	}
	if !f.called("GET /_ping") || !f.called("GET /images/sre-agent:local/json") {
		t.Errorf("probe skipped ping or image check: %q", f.callList())
	}
	if left, _ := os.ReadDir(probeDir); len(left) != 0 {
		t.Errorf("probe left %d entries in ProbeDir", len(left))
	}

	// A container that runs but does not see the mounted file is not available.
	blind := &fakeEngine{onCreate: func(*testing.T, createBody) []byte { return frame(1, "something else\n") }}
	d2 := NewDocker("sre-agent:local", probeDir)
	d2.Socket = blind.start(t)
	if d2.Available(context.Background()) {
		t.Fatal("Available = true for a container that never read the probe file")
	}
	if err := d2.ProbeError(); err == nil || !strings.Contains(err.Error(), "bind mount") {
		t.Errorf("ProbeError = %v, want the bind mount named", err)
	}

	noImage := &fakeEngine{missingImage: true, onCreate: echoProbe(probeDir)}
	d3 := NewDocker("sre-agent:local", probeDir)
	d3.Socket = noImage.start(t)
	if d3.Available(context.Background()) {
		t.Fatal("Available = true without the image")
	}
	if err := d3.ProbeError(); err == nil || !strings.Contains(err.Error(), "sre-agent:local") {
		t.Errorf("ProbeError = %v, want the image named", err)
	}
}

// The version comes from an unversioned /_ping, which any daemon answers, and
// every later call uses min(daemon, 1.47). A daemon older than 1.44, or one that
// does not say, is unavailable with the reason. Versions compare as numbers.
func TestDockerNegotiatesAPIVersion(t *testing.T) {
	for _, tc := range []struct {
		name, server string
		noHeader     bool
		wantAPI      string // "" means unavailable
		wantReason   string
	}{
		{name: "older than 1.44", server: "1.43", wantReason: "1.43"},
		{name: "1.5 is older than 1.44", server: "1.5", wantReason: "1.5"},
		{name: "oldest supported", server: "1.44", wantAPI: "/v1.44"},
		{name: "newer than 1.47", server: "1.51", wantAPI: "/v1.47"},
		{name: "1.100 is newer than 1.47", server: "1.100", wantAPI: "/v1.47"},
		{name: "no header", noHeader: true, wantReason: "Api-Version"},
		{name: "unparsable header", server: "latest", wantReason: "latest"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			probeDir := t.TempDir()
			f := &fakeEngine{apiVersion: tc.server, noAPIVersion: tc.noHeader, wantAPI: tc.wantAPI, onCreate: echoProbe(probeDir)}
			d := NewDocker("sre-agent:local", probeDir)
			d.Socket = f.start(t)
			if err := d.ProbeError(); err != nil {
				t.Fatalf("ProbeError = %v before probing", err)
			}

			got := d.Available(context.Background())
			if tc.wantAPI != "" {
				if !got {
					t.Fatalf("Available = false: %v; calls = %q", d.ProbeError(), f.callList())
				}
				if !f.called("POST /containers/c1/wait") {
					t.Fatalf("probe never ran a container: %q", f.callList())
				}
				return
			}
			if got {
				t.Fatal("Available = true")
			}
			if err := d.ProbeError(); err == nil || !strings.Contains(err.Error(), tc.wantReason) {
				t.Errorf("ProbeError = %v, want it to mention %q", err, tc.wantReason)
			}
			if calls := f.callList(); !slices.Equal(calls, []string{"GET /_ping"}) {
				t.Errorf("calls = %q, want only the negotiating ping", calls)
			}
		})
	}
}

func TestDockerUnavailableWithoutSocket(t *testing.T) {
	d := NewDocker("sre-agent:local", t.TempDir())
	d.Socket = "/nonexistent.sock"
	start := time.Now()
	if d.Available(context.Background()) {
		t.Fatal("Available = true without a socket")
	}
	if el := time.Since(start); el > time.Second {
		t.Fatalf("took %v", el)
	}
	if err := d.ProbeError(); err == nil || !strings.Contains(err.Error(), "/nonexistent.sock") {
		t.Errorf("ProbeError = %v, want the socket named", err)
	}
}

// The probe runs at startup before Slack connects, so a daemon that never
// finishes a container must not hold the bot past the probe timeout plus cleanup.
func TestDockerAvailableDoesNotHangOnAStuckDaemon(t *testing.T) {
	f := &fakeEngine{blockWait: true}
	d := NewDocker("sre-agent:local", t.TempDir())
	d.Socket = f.start(t)
	d.probeTimeout = 200 * time.Millisecond

	done := make(chan bool, 1)
	go func() { done <- d.Available(context.Background()) }()
	select {
	case ok := <-done:
		if ok {
			t.Fatal("Available = true for a container that never exited")
		}
		if d.ProbeError() == nil {
			t.Error("ProbeError = nil for a probe that timed out")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Available still blocked 5s after a 200ms probe timeout")
	}
	if !f.called("DELETE /containers/c1?force=1") {
		t.Fatalf("probe container not removed: %q", f.callList())
	}
}

func TestNewDockerSocket(t *testing.T) {
	for _, tc := range []struct{ host, want string }{
		{"", "/var/run/docker.sock"},
		{"unix:///Users/me/.docker/run/docker.sock", "/Users/me/.docker/run/docker.sock"},
		{"tcp://127.0.0.1:2375", "/var/run/docker.sock"},
	} {
		t.Setenv("DOCKER_HOST", tc.host)
		d := NewDocker("img", "/w")
		if d.Socket != tc.want || d.Timeout != 60*time.Second || d.MaxOutput != 16384 || d.Name() != "docker" {
			t.Errorf("DOCKER_HOST=%q: %+v", tc.host, d)
		}
	}
}
