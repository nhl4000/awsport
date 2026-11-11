package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nhl4000/awsport/internal/core"
	"github.com/nhl4000/awsport/internal/tunnel"
)

const helperEnv = "GO_WANT_AGENT_HELPER"

// TestHelperProcess is not a real test: fake tunnel sessions re-exec the
// test binary into it.
func TestHelperProcess(t *testing.T) {
	if os.Getenv(helperEnv) == "" {
		return
	}
	// Safety net: never outlive a crashed test run.
	go func() {
		time.Sleep(30 * time.Second)
		os.Exit(3)
	}()
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt)
	fmt.Println("Waiting for connections...")
	<-ch
	os.Exit(0)
}

func fakeCommand(ctx context.Context, _ core.TunnelSpec) *exec.Cmd {
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestHelperProcess$")
	// atexit_sleep_ms=0: race-enabled helpers otherwise linger 1s on exit.
	cmd.Env = append(os.Environ(), helperEnv+"=up", "GORACE=atexit_sleep_ms=0")
	return cmd
}

// socketPath returns a socket path in a fresh short directory (unix socket
// paths are limited to ~100 bytes, too little for t.TempDir on some systems).
func socketPath(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "awsport-agent")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, socketName)
}

type testAgent struct {
	srv    *Server
	cancel context.CancelFunc
	done   chan struct{}
	err    error
}

func runAgent(t *testing.T, sock, version string, idle time.Duration) *testAgent {
	t.Helper()
	ln, err := Listen(sock)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	ta := &testAgent{
		srv: &Server{
			Manager:     tunnel.NewManager(tunnel.Options{AWSPath: "aws", Command: fakeCommand}),
			Version:     version,
			IdleTimeout: idle,
		},
		done: make(chan struct{}),
	}
	ctx, cancel := context.WithCancel(context.Background())
	ta.cancel = cancel
	go func() {
		ta.err = ta.srv.Serve(ctx, ln)
		close(ta.done)
	}()
	t.Cleanup(func() {
		cancel()
		ta.wait(t)
	})
	return ta
}

func (ta *testAgent) exited() bool {
	select {
	case <-ta.done:
		return true
	default:
		return false
	}
}

func (ta *testAgent) wait(t *testing.T) error {
	t.Helper()
	select {
	case <-ta.done:
		return ta.err
	case <-time.After(15 * time.Second):
		t.Fatal("agent did not exit")
		return nil
	}
}

func testClient(t *testing.T, sock, version string) *Client {
	t.Helper()
	c := newClient(sock, version)
	c.spawn = func() error {
		t.Error("unexpected agent spawn")
		return errors.New("spawn disabled")
	}
	t.Cleanup(c.Close)
	return c
}

func testSpec(key string) core.TunnelSpec {
	return core.TunnelSpec{
		Key: key, Label: "app " + key, Profile: "example", Region: "us-east-1",
		Target: "i-0123456789abcdef0", RemotePort: 8080, LocalPort: 18080,
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// nextEvent returns the next event for key from ch.
func nextEvent(t *testing.T, ch <-chan tunnel.Event, key string) tunnel.Event {
	t.Helper()
	timeout := time.After(15 * time.Second)
	for {
		select {
		case ev := <-ch:
			if ev.Key == key {
				return ev
			}
		case <-timeout:
			t.Fatalf("no event for %s", key)
		}
	}
}

func waitEventStatus(t *testing.T, ch <-chan tunnel.Event, key string, want tunnel.Status) {
	t.Helper()
	for {
		if ev := nextEvent(t, ch, key); ev.Session.Status == want {
			return
		}
	}
}

func status(c *Client, key string) tunnel.Status {
	s, ok := c.Get(key)
	if !ok {
		return ""
	}
	return s.Status
}

func TestClientServerRoundTrip(t *testing.T) {
	sock := socketPath(t)
	runAgent(t, sock, "test", time.Minute)
	c := testClient(t, sock, "test")
	if s := c.Sessions(); len(s) != 0 {
		t.Fatalf("Sessions = %v, want none", s)
	}
	events := c.Events()

	spec := testSpec("example/us-east-1/i-0123456789abcdef0/8080")
	if err := c.Start(spec); err != nil {
		t.Fatalf("Start: %v", err)
	}
	// Start returns once the mirror reflects the new session.
	if s, ok := c.Get(spec.Key); !ok || s.Spec != spec {
		t.Fatalf("Get after Start = %+v, %v", s, ok)
	}
	waitEventStatus(t, events, spec.Key, tunnel.Up)
	if st := status(c, spec.Key); st != tunnel.Up {
		t.Fatalf("mirrored status = %q, want up", st)
	}
	if n := c.ActiveCount(); n != 1 {
		t.Fatalf("ActiveCount = %d, want 1", n)
	}
	if err := c.Start(spec); err == nil {
		t.Fatal("starting an active key must fail")
	}

	// A second client sees the running session on its first read.
	c2 := testClient(t, sock, "test")
	if s := c2.Sessions(); len(s) != 1 || s[0].Status != tunnel.Up || s[0].Spec != spec {
		t.Fatalf("second client Sessions = %+v", s)
	}
	if w := c2.Warning(); w != "" {
		t.Fatalf("Warning = %q, want none", w)
	}

	if err := c.Stop(spec.Key); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if _, ok := c.Get(spec.Key); ok {
		t.Fatal("session still mirrored after Stop")
	}
	waitEventStatus(t, events, spec.Key, tunnel.Stopped)
	waitFor(t, "second client to drop the session", func() bool { return len(c2.Sessions()) == 0 })
	if err := c.Restart(spec.Key); err == nil {
		t.Fatal("Restart of a stopped key must fail")
	}
}

func TestClientRestartAndStopAll(t *testing.T) {
	sock := socketPath(t)
	runAgent(t, sock, "test", time.Minute)
	c := testClient(t, sock, "test")
	a, b := testSpec("a"), testSpec("b")
	b.LocalPort++
	for _, s := range []core.TunnelSpec{a, b} {
		if err := c.Start(s); err != nil {
			t.Fatal(err)
		}
	}
	waitFor(t, "both up", func() bool { return status(c, "a") == tunnel.Up && status(c, "b") == tunnel.Up })
	if err := c.Restart("a"); err != nil {
		t.Fatalf("Restart: %v", err)
	}
	waitFor(t, "restarted session up", func() bool { return status(c, "a") == tunnel.Up })
	if keys, err := c.RestartAuthExpired(); err != nil || len(keys) != 0 {
		t.Fatalf("RestartAuthExpired = %v, %v", keys, err)
	}
	if err := c.StopAll(); err != nil {
		t.Fatalf("StopAll: %v", err)
	}
	if s := c.Sessions(); len(s) != 0 {
		t.Fatalf("Sessions after StopAll = %+v", s)
	}
}

// fakeAgent serves /v1/info and /v1/events with scripted snapshots: stream i
// sends snapshots[i] and ends when drops[i] is closed.
type fakeAgent struct {
	ln        net.Listener
	srv       *http.Server
	snapshots [][]tunnel.Session
	drops     []chan struct{}
	streams   atomic.Int32
}

func runFakeAgent(t *testing.T, sock string, snapshots ...[]tunnel.Session) *fakeAgent {
	t.Helper()
	ln, err := Listen(sock)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeAgent{ln: ln, snapshots: snapshots}
	for range snapshots {
		f.drops = append(f.drops, make(chan struct{}))
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/info", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, Info{Version: "test", Sessions: len(snapshots[0])})
	})
	mux.HandleFunc("GET /v1/events", func(w http.ResponseWriter, r *http.Request) {
		i := int(f.streams.Add(1)) - 1
		if i >= len(f.snapshots) {
			writeError(w, http.StatusServiceUnavailable, errShuttingDown)
			return
		}
		snap := f.snapshots[i]
		var rev uint64
		for _, s := range snap {
			rev = max(rev, s.Rev)
		}
		w.Header().Set(headerSnapshot, strconv.Itoa(len(snap)))
		w.Header().Set(headerRev, strconv.FormatUint(rev, 10))
		enc := json.NewEncoder(w)
		for _, s := range snap {
			_ = enc.Encode(tunnel.Event{Key: s.Spec.Key, Session: s})
		}
		w.(http.Flusher).Flush()
		select {
		case <-f.drops[i]:
		case <-r.Context().Done():
		}
	})
	f.srv = &http.Server{Handler: mux}
	go func() { _ = f.srv.Serve(ln) }()
	t.Cleanup(func() {
		for _, d := range f.drops {
			select {
			case <-d:
			default:
				close(d)
			}
		}
		_ = f.srv.Close()
	})
	return f
}

func TestStreamDropReconnectsThenMarksStopped(t *testing.T) {
	sock := socketPath(t)
	up := func(key string, rev uint64) tunnel.Session {
		return tunnel.Session{Spec: testSpec(key), Status: tunnel.Up, Rev: rev}
	}
	f := runFakeAgent(t, sock, []tunnel.Session{up("a", 3)}, []tunnel.Session{up("b", 7)})
	c := testClient(t, sock, "test")
	if s := c.Sessions(); len(s) != 1 || s[0].Spec.Key != "a" {
		t.Fatalf("first Sessions = %+v, want a", s)
	}
	events := c.Events()

	// The agent is alive: the client reopens the stream and reconciles.
	close(f.drops[0])
	got := map[string]tunnel.Status{}
	for range 2 {
		select {
		case ev := <-events:
			got[ev.Key] = ev.Session.Status
		case <-time.After(15 * time.Second):
			t.Fatalf("reconnect events = %v", got)
		}
	}
	if got["a"] != tunnel.Stopped || got["b"] != tunnel.Up {
		t.Fatalf("reconnect events = %v, want a stopped, b up", got)
	}
	if s := c.Sessions(); len(s) != 1 || s[0].Spec.Key != "b" {
		t.Fatalf("Sessions after reconnect = %+v, want b", s)
	}

	// The agent is gone: every mirrored session is reported Stopped.
	_ = os.Remove(sock)
	_ = f.ln.Close()
	close(f.drops[1])
	if ev := nextEvent(t, events, "b"); ev.Session.Status != tunnel.Stopped {
		t.Fatalf("b event = %s, want stopped", ev.Session.Status)
	}
	if s := c.Sessions(); len(s) != 0 {
		t.Fatalf("Sessions after agent loss = %+v", s)
	}
}

func TestAgentShutdownStopsSessions(t *testing.T) {
	sock := socketPath(t)
	ta := runAgent(t, sock, "test", time.Minute)
	c := testClient(t, sock, "test")
	if err := c.Start(testSpec("a")); err != nil {
		t.Fatal(err)
	}
	events := c.Events()
	waitEventStatus(t, events, "a", tunnel.Up)
	ta.cancel()
	if err := ta.wait(t); err != nil {
		t.Fatalf("Serve: %v", err)
	}
	waitEventStatus(t, events, "a", tunnel.Stopped)
	if _, err := os.Stat(sock); !os.IsNotExist(err) {
		t.Fatalf("socket not removed: %v", err)
	}
	if s := c.Sessions(); len(s) != 0 {
		t.Fatalf("Sessions = %+v, want none", s)
	}
}

func TestIdleExit(t *testing.T) {
	sock := socketPath(t)
	ta := runAgent(t, sock, "test", 50*time.Millisecond)
	if err := ta.wait(t); err != nil {
		t.Fatalf("Serve: %v", err)
	}
	if _, err := os.Stat(sock); !os.IsNotExist(err) {
		t.Fatalf("socket not removed: %v", err)
	}

	// Sessions keep the agent alive; it exits once they are gone.
	ta = runAgent(t, sock, "test", 200*time.Millisecond)
	c := testClient(t, sock, "test")
	if err := c.Start(testSpec("a")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "session up", func() bool { return status(c, "a") == tunnel.Up })
	time.Sleep(500 * time.Millisecond)
	if ta.exited() {
		t.Fatal("agent exited while a session was up")
	}
	if err := c.Stop("a"); err != nil {
		t.Fatal(err)
	}
	if err := ta.wait(t); err != nil {
		t.Fatalf("Serve: %v", err)
	}
}

func TestListenReplacesStaleSocket(t *testing.T) {
	sock := socketPath(t)
	stale, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	stale.(*net.UnixListener).SetUnlinkOnClose(false)
	_ = stale.Close()
	if _, err := os.Stat(sock); err != nil {
		t.Fatalf("stale socket missing: %v", err)
	}
	ln, err := Listen(sock)
	if err != nil {
		t.Fatalf("Listen over stale socket: %v", err)
	}
	defer ln.Close()
	if fi, err := os.Stat(sock); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("socket mode = %v, %v; want 0600", fi.Mode().Perm(), err)
	}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()
	if _, err := Listen(sock); !errors.Is(err, ErrRunning) {
		t.Fatalf("Listen over live socket = %v, want ErrRunning", err)
	}
}

func TestReadsNeverSpawn(t *testing.T) {
	sock := socketPath(t)
	c := testClient(t, sock, "test")
	if s := c.Sessions(); len(s) != 0 {
		t.Fatalf("Sessions = %+v", s)
	}
	if _, ok := c.Get("a"); ok {
		t.Fatal("Get found a session without agent")
	}
	if c.ActiveCount() != 0 || c.Warning() != "" {
		t.Fatal("unexpected state without agent")
	}
	if err := c.Stop("a"); err != nil {
		t.Fatalf("Stop without agent: %v", err)
	}
	if err := c.StopAll(); err != nil {
		t.Fatalf("StopAll without agent: %v", err)
	}
	if keys, err := c.RestartAuthExpired(); err != nil || keys != nil {
		t.Fatalf("RestartAuthExpired without agent = %v, %v", keys, err)
	}
}

func TestStartSpawnsAgent(t *testing.T) {
	sock := socketPath(t)
	c := testClient(t, sock, "test")
	var spawns atomic.Int32
	c.spawn = func() error {
		spawns.Add(1)
		runAgent(t, sock, "test", time.Minute)
		return nil
	}
	_ = c.Events()
	if err := c.Start(testSpec("a")); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if n := spawns.Load(); n != 1 {
		t.Fatalf("spawns = %d, want 1", n)
	}
	if _, ok := c.Get("a"); !ok {
		t.Fatal("session not mirrored after Start")
	}
}

func TestOutdatedAgent(t *testing.T) {
	t.Run("idle agent is replaced", func(t *testing.T) {
		sock := socketPath(t)
		old := runAgent(t, sock, "v1", time.Minute)
		c := testClient(t, sock, "v2")
		var mu sync.Mutex
		var fresh *testAgent
		c.spawn = func() error {
			mu.Lock()
			defer mu.Unlock()
			fresh = runAgent(t, sock, "v2", time.Minute)
			return nil
		}
		if s := c.Sessions(); len(s) != 0 || old.exited() {
			t.Fatal("reads must not replace the agent")
		}
		if err := c.Start(testSpec("a")); err != nil {
			t.Fatalf("Start: %v", err)
		}
		if !old.exited() {
			t.Fatal("outdated agent still running")
		}
		mu.Lock()
		defer mu.Unlock()
		if fresh == nil || len(fresh.srv.Manager.Sessions()) != 1 {
			t.Fatal("session not started on the fresh agent")
		}
		if w := c.Warning(); w != "" {
			t.Fatalf("Warning = %q", w)
		}
	})
	t.Run("busy agent is kept", func(t *testing.T) {
		sock := socketPath(t)
		runAgent(t, sock, "v1", time.Minute)
		if err := testClient(t, sock, "v1").Start(testSpec("a")); err != nil {
			t.Fatal(err)
		}
		c := testClient(t, sock, "v2")
		if s := c.Sessions(); len(s) != 1 {
			t.Fatalf("Sessions = %+v, want the running session", s)
		}
		if w := c.Warning(); w == "" {
			t.Fatal("expected a version warning")
		}
	})
}

func TestDecideVersion(t *testing.T) {
	tests := []struct {
		agent, client string
		sessions      int
		want          versionAction
	}{
		{"v1.2.0", "v1.2.0", 0, useAgent},
		{"v1.2.0", "v1.2.0", 3, useAgent},
		{"v1.1.0", "v1.2.0", 0, replaceAgent},
		{"dev", "v1.2.0", 0, replaceAgent},
		{"v1.1.0", "v1.2.0", 2, keepAgent},
	}
	for _, tt := range tests {
		if got := decideVersion(tt.agent, tt.client, tt.sessions); got != tt.want {
			t.Errorf("decideVersion(%q, %q, %d) = %d, want %d", tt.agent, tt.client, tt.sessions, got, tt.want)
		}
	}
}
