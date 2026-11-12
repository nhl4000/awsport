package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/nhl4000/awsport/internal/core"
	"github.com/nhl4000/awsport/internal/tunnel"
)

const (
	baseURL = "http://awsport-agent"
	// spawnTimeout bounds waiting for a spawned agent to listen and for a
	// replaced one to go away.
	spawnTimeout = 5 * time.Second
	// requestTimeout bounds quick requests and loading the stream snapshot.
	requestTimeout = 5 * time.Second
	// mutationTimeout bounds requests that may stop sessions (each stop waits
	// up to the Manager's kill grace period).
	mutationTimeout = 30 * time.Second
	// syncTimeout bounds waiting for the event stream to reflect a change.
	syncTimeout = 2 * time.Second
	// probeInterval spaces read-triggered checks for an agent that was absent.
	probeInterval = 2 * time.Second
	// redialAttempts bounds reopening a dropped stream to a live agent.
	redialAttempts = 5
	redialDelay    = 100 * time.Millisecond
	clientEvents   = 256
)

var (
	errNotRunning = errors.New("agent is not running")
	errClosed     = errors.New("agent client closed")
)

// Client is a tunnel.Controller backed by the background agent. Reads are
// served from a local mirror that an event stream keeps current; they connect
// to an agent that is already running but never start one. Start and Restart
// start an agent when none is running.
type Client struct {
	version string
	sock    string
	sockErr error
	spawn   func() error // starts an agent process (test seam)

	hc   *http.Client
	ctx  context.Context // cancelled by Close
	stop context.CancelFunc

	events chan tunnel.Event

	connMu sync.Mutex // serializes connecting and reopening the stream

	mu        sync.Mutex
	mirror    map[string]tunnel.Session
	streaming bool          // the stream goroutine runs
	connected bool          // a stream was established at least once
	probed    bool          // a read-triggered connect has been attempted
	lastProbe time.Time     // last failed read-triggered connect
	seen      uint64        // highest Rev the mirror reflects on the current stream
	gen       int           // incremented per established stream
	changed   chan struct{} // closed and replaced when seen, gen or streaming change
	done      chan struct{} // closed when the stream goroutine exits
	warning   string
}

var _ tunnel.Controller = (*Client)(nil)

// NewClient returns a client for the agent socket in the state directory.
// It connects lazily.
func NewClient(version string) *Client {
	sock, err := SocketPath()
	c := newClient(sock, version)
	c.sockErr = err
	return c
}

func newClient(sock, version string) *Client {
	ctx, stop := context.WithCancel(context.Background())
	c := &Client{
		version: version,
		sock:    sock,
		ctx:     ctx,
		stop:    stop,
		events:  make(chan tunnel.Event, clientEvents),
		mirror:  map[string]tunnel.Session{},
		changed: make(chan struct{}),
	}
	c.spawn = func() error { return spawnAgent(filepath.Dir(sock)) }
	c.hc = &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", sock)
		},
		DisableCompression: true,
		// One connection per request: a reused idle connection could reach an
		// agent that is shutting down after a fresh one took over the socket
		// (observed on Windows). Local socket dials cost microseconds.
		DisableKeepAlives: true,
	}}
	return c
}

// spawnAgent re-executes this binary as a detached `awsport agent`.
func spawnAgent(dir string) error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate awsport executable: %w", err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	// Nil stdio means the null device. The state directory as working
	// directory keeps the agent from pinning the caller's directory.
	cmd := exec.Command(exe, "agent")
	cmd.Dir = dir
	detach(cmd)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start agent: %w", err)
	}
	// Reap the agent if it exits while this process still runs.
	go func() { _ = cmd.Wait() }()
	return nil
}

// Warning describes a version mismatch with a running agent that is kept
// because it owns sessions; empty otherwise.
func (c *Client) Warning() string {
	c.probe()
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.warning
}

// Sessions returns the mirrored sessions sorted by Label (then Key).
func (c *Client) Sessions() []tunnel.Session {
	c.probe()
	c.mu.Lock()
	out := slices.Collect(maps.Values(c.mirror))
	c.mu.Unlock()
	tunnel.SortSessions(out)
	return out
}

// Get returns the mirrored session with key.
func (c *Client) Get(key string) (tunnel.Session, bool) {
	c.probe()
	c.mu.Lock()
	defer c.mu.Unlock()
	s, ok := c.mirror[key]
	return s, ok
}

// ActiveCount counts mirrored sessions that are Starting, Up or Reconnecting.
func (c *Client) ActiveCount() int {
	c.probe()
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, s := range c.mirror {
		if tunnel.IsActive(s.Status) {
			n++
		}
	}
	return n
}

// Events delivers session changes forwarded from the agent, plus Stopped
// events for every session when the agent goes away. The channel is
// buffered; when the consumer lags, the oldest pending events are dropped.
func (c *Client) Events() <-chan tunnel.Event {
	c.probe()
	return c.events
}

// Start starts a tunnel in the agent, starting the agent if needed.
func (c *Client) Start(spec core.TunnelSpec) error {
	_, err := c.mutate(true, "/v1/start", spec)
	return err
}

// Stop stops the session with key. Unknown keys are ignored.
func (c *Client) Stop(key string) error {
	_, err := c.mutate(false, "/v1/stop", keyRequest{Key: key})
	return err
}

// StopAll stops every session of the agent.
func (c *Client) StopAll() error {
	_, err := c.mutate(false, "/v1/stopall", nil)
	return err
}

// Restart restarts the session with key, starting the agent if needed.
func (c *Client) Restart(key string) error {
	_, err := c.mutate(true, "/v1/restart", keyRequest{Key: key})
	return err
}

// RestartAuthExpired restarts every AuthExpired session and returns the
// restarted keys.
func (c *Client) RestartAuthExpired() ([]string, error) {
	resp, err := c.mutate(false, "/v1/restart-auth", nil)
	return resp.Keys, err
}

// Close ends the event stream. It does not affect the agent or its tunnels.
func (c *Client) Close() {
	c.stop()
	c.mu.Lock()
	done := c.done
	c.mu.Unlock()
	if done != nil {
		<-done
	}
	c.hc.CloseIdleConnections()
}

// probe connects for reads. It never starts an agent; the first call waits
// for the connection, later calls skip while another connect is in progress
// and re-check an absent agent at most every probeInterval.
func (c *Client) probe() {
	c.mu.Lock()
	skip := c.streaming || (c.probed && time.Since(c.lastProbe) < probeInterval)
	first := !c.probed
	c.mu.Unlock()
	if skip {
		return
	}
	if first {
		c.connMu.Lock()
	} else if !c.connMu.TryLock() {
		return
	}
	defer c.connMu.Unlock()
	err := c.connect(false)
	c.mu.Lock()
	c.probed = true
	if err != nil {
		c.lastProbe = time.Now()
	}
	c.mu.Unlock()
}

// mutate sends a state-changing request and waits until the mirror reflects
// it. With spawn, a missing agent is started; without, a missing agent makes
// the call a successful no-op.
func (c *Client) mutate(spawn bool, path string, in any) (mutationResponse, error) {
	for attempt := 0; ; attempt++ {
		c.connMu.Lock()
		err := c.connect(spawn)
		c.connMu.Unlock()
		if errors.Is(err, errNotRunning) && !spawn {
			return mutationResponse{}, nil
		}
		if err != nil {
			return mutationResponse{}, err
		}
		c.mu.Lock()
		gen := c.gen
		c.mu.Unlock()
		var resp mutationResponse
		err = c.call(http.MethodPost, path, in, &resp, mutationTimeout)
		if err == nil {
			c.waitSeen(resp.Rev)
			return resp, nil
		}
		if !spawn || attempt >= maxShutdownRetries || !isStatus(err, http.StatusServiceUnavailable) {
			return resp, err
		}
		// The agent is exiting: wait for the stream to notice, then retry
		// against a fresh agent.
		c.waitStreamChange(gen)
	}
}

// maxShutdownRetries bounds retries of a mutation that hit an exiting agent.
const maxShutdownRetries = 3

// connect makes sure the event stream runs, starting an agent first when
// spawn is set. The caller holds connMu.
func (c *Client) connect(spawn bool) error {
	if c.ctx.Err() != nil {
		return errClosed
	}
	c.mu.Lock()
	streaming := c.streaming
	c.mu.Unlock()
	if streaming {
		return nil
	}
	if c.sockErr != nil {
		return c.sockErr
	}
	if !dialable(c.sock) {
		if !spawn {
			return errNotRunning
		}
		if err := c.startAgent(); err != nil {
			return err
		}
	} else if err := c.handshake(spawn); err != nil {
		return err
	}
	st, err := c.openStream()
	if err != nil {
		return err
	}
	c.mu.Lock()
	c.streaming = true
	c.done = make(chan struct{})
	done := c.done
	c.mu.Unlock()
	go c.readStream(st, done)
	return nil
}

type versionAction int

const (
	useAgent     versionAction = iota // same version
	replaceAgent                      // other version without sessions: start a fresh agent
	keepAgent                         // other version owning sessions: keep using it
)

// decideVersion picks how to treat a running agent of agentVersion.
func decideVersion(agentVersion, clientVersion string, sessions int) versionAction {
	switch {
	case agentVersion == clientVersion:
		return useAgent
	case sessions == 0:
		return replaceAgent
	default:
		return keepAgent
	}
}

// handshake checks the running agent's version. An outdated agent without
// sessions is replaced when spawn is set and otherwise treated as absent.
func (c *Client) handshake(spawn bool) error {
	var info Info
	if err := c.call(http.MethodGet, "/v1/info", nil, &info, requestTimeout); err != nil {
		return err
	}
	warning := ""
	switch decideVersion(info.Version, c.version, info.Sessions) {
	case replaceAgent:
		if !spawn {
			return errNotRunning
		}
		if err := c.call(http.MethodPost, "/v1/shutdown", nil, nil, requestTimeout); err != nil {
			return fmt.Errorf("stop outdated agent %s: %w", info.Version, err)
		}
		if err := c.waitGone(); err != nil {
			return err
		}
		if err := c.startAgent(); err != nil {
			return err
		}
	case keepAgent:
		warning = fmt.Sprintf("background agent runs awsport %s (this is %s); it is replaced once all tunnels are stopped",
			info.Version, c.version)
	}
	c.mu.Lock()
	c.warning = warning
	c.mu.Unlock()
	return nil
}

// startAgent spawns an agent and waits until it listens.
func (c *Client) startAgent() error {
	if err := c.spawn(); err != nil {
		return err
	}
	deadline := time.Now().Add(spawnTimeout)
	for !dialable(c.sock) {
		if time.Now().After(deadline) {
			return fmt.Errorf("agent did not start within %s (see %s)",
				spawnTimeout, filepath.Join(filepath.Dir(c.sock), logName))
		}
		if !c.sleep(25 * time.Millisecond) {
			return errClosed
		}
	}
	return nil
}

func (c *Client) waitGone() error {
	deadline := time.Now().Add(spawnTimeout)
	for dialable(c.sock) {
		if time.Now().After(deadline) {
			return fmt.Errorf("outdated agent did not exit within %s", spawnTimeout)
		}
		if !c.sleep(25 * time.Millisecond) {
			return errClosed
		}
	}
	return nil
}

func (c *Client) sleep(d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-c.ctx.Done():
		return false
	}
}

type stream struct {
	body   io.ReadCloser
	dec    *json.Decoder
	cancel context.CancelFunc
}

func (st stream) close() {
	st.cancel()
	_ = st.body.Close()
}

// openStream opens GET /v1/events and syncs the mirror to its snapshot.
func (c *Client) openStream() (stream, error) {
	ctx, cancel := context.WithCancel(c.ctx)
	// A stalled agent must not hang the caller while the snapshot loads.
	timer := time.AfterFunc(requestTimeout, cancel)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/v1/events", nil)
	if err != nil {
		timer.Stop()
		cancel()
		return stream{}, err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		timer.Stop()
		cancel()
		return stream{}, fmt.Errorf("agent: %w", err)
	}
	st := stream{body: resp.Body, dec: json.NewDecoder(resp.Body), cancel: cancel}
	fail := func(err error) (stream, error) {
		timer.Stop()
		st.close()
		return stream{}, err
	}
	if resp.StatusCode != http.StatusOK {
		return fail(responseError(resp))
	}
	n, err := strconv.Atoi(resp.Header.Get(headerSnapshot))
	if err != nil {
		return fail(fmt.Errorf("agent: bad %s header: %w", headerSnapshot, err))
	}
	rev, err := strconv.ParseUint(resp.Header.Get(headerRev), 10, 64)
	if err != nil {
		return fail(fmt.Errorf("agent: bad %s header: %w", headerRev, err))
	}
	snap := make([]tunnel.Event, 0, n)
	for range n {
		var ev tunnel.Event
		if err := st.dec.Decode(&ev); err != nil {
			return fail(fmt.Errorf("agent: read snapshot: %w", err))
		}
		snap = append(snap, ev)
	}
	if !timer.Stop() {
		return fail(fmt.Errorf("agent: snapshot not received within %s", requestTimeout))
	}
	c.applySnapshot(snap, rev)
	return st, nil
}

// applySnapshot replaces the mirror. On the first stream the snapshot is the
// baseline readers see directly; on later ones vanished, then new and
// changed sessions are reported as events.
func (c *Client) applySnapshot(snap []tunnel.Event, rev uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	report := c.connected
	old := c.mirror
	c.mirror = make(map[string]tunnel.Session, len(snap))
	for _, ev := range snap {
		c.mirror[ev.Key] = ev.Session
	}
	if report {
		now := time.Now()
		for _, k := range slices.Sorted(maps.Keys(old)) {
			if _, ok := c.mirror[k]; !ok {
				c.emitLocked(stoppedEvent(k, old[k], now))
			}
		}
		for _, ev := range snap {
			if prev, ok := old[ev.Key]; !ok || prev.Rev != ev.Session.Rev || prev.Status != ev.Session.Status {
				c.emitLocked(ev)
			}
		}
	}
	c.connected = true
	c.seen = rev
	c.gen++
	c.notifyLocked()
}

// readStream applies stream events to the mirror. When the stream drops it
// reopens it while the agent is reachable; once the agent is gone every
// mirrored session is reported Stopped.
func (c *Client) readStream(st stream, done chan struct{}) {
	defer close(done)
	for {
		for {
			var ev tunnel.Event
			if err := st.dec.Decode(&ev); err != nil {
				break
			}
			c.apply(ev)
		}
		st.close()
		if c.ctx.Err() != nil {
			c.endStream(false)
			return
		}
		c.connMu.Lock()
		next, err := c.redial()
		if err != nil {
			c.endStream(true)
		}
		c.connMu.Unlock()
		if err != nil {
			return
		}
		st = next
	}
}

func (c *Client) redial() (stream, error) {
	for attempt := 1; ; attempt++ {
		if !dialable(c.sock) {
			return stream{}, errNotRunning
		}
		st, err := c.openStream()
		if err == nil {
			return st, nil
		}
		if attempt == redialAttempts || !c.sleep(redialDelay) {
			return stream{}, err
		}
	}
}

func (c *Client) apply(ev tunnel.Event) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if ev.Session.Status == tunnel.Stopped {
		delete(c.mirror, ev.Key)
	} else {
		c.mirror[ev.Key] = ev.Session
	}
	c.seen = max(c.seen, ev.Session.Rev)
	c.emitLocked(ev)
	c.notifyLocked()
}

// endStream marks the stream as gone; with lost, the agent is gone too and
// every mirrored session is reported Stopped.
func (c *Client) endStream(lost bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.streaming = false
	if lost {
		now := time.Now()
		for _, k := range slices.Sorted(maps.Keys(c.mirror)) {
			c.emitLocked(stoppedEvent(k, c.mirror[k], now))
		}
		clear(c.mirror)
	}
	c.notifyLocked()
}

func stoppedEvent(key string, s tunnel.Session, now time.Time) tunnel.Event {
	s.Status = tunnel.Stopped
	s.Since = now
	return tunnel.Event{Key: key, Session: s}
}

// emitLocked sends ev without blocking, dropping the oldest queued event when
// the buffer is full. Callers hold c.mu, which keeps events ordered.
func (c *Client) emitLocked(ev tunnel.Event) {
	for {
		select {
		case c.events <- ev:
			return
		default:
		}
		select {
		case <-c.events:
		default:
		}
	}
}

func (c *Client) notifyLocked() {
	close(c.changed)
	c.changed = make(chan struct{})
}

// waitSeen waits until the mirror reflects rev, the stream ends or
// syncTimeout passes.
func (c *Client) waitSeen(rev uint64) {
	c.waitUntil(func() bool { return c.seen >= rev || !c.streaming })
}

// waitStreamChange waits until the stream of generation gen was replaced or
// ended.
func (c *Client) waitStreamChange(gen int) {
	c.waitUntil(func() bool { return c.gen != gen || !c.streaming })
}

// waitUntil waits up to syncTimeout for cond, evaluated under c.mu.
func (c *Client) waitUntil(cond func() bool) {
	t := time.NewTimer(syncTimeout)
	defer t.Stop()
	for {
		c.mu.Lock()
		ok, ch := cond(), c.changed
		c.mu.Unlock()
		if ok {
			return
		}
		select {
		case <-ch:
		case <-t.C:
			return
		case <-c.ctx.Done():
			return
		}
	}
}

type statusError struct {
	code int
	msg  string
}

func (e *statusError) Error() string { return e.msg }

func isStatus(err error, code int) bool {
	var se *statusError
	return errors.As(err, &se) && se.code == code
}

func responseError(resp *http.Response) error {
	var e errorResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxRequestBody)).Decode(&e); err != nil || e.Error == "" {
		e.Error = "agent: " + resp.Status
	}
	return &statusError{code: resp.StatusCode, msg: e.Error}
}

// call sends one JSON request and decodes the JSON answer into out.
func (c *Client) call(method, path string, in, out any, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(c.ctx, timeout)
	defer cancel()
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return fmt.Errorf("encode %s request: %w", path, err)
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, baseURL+path, body)
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("agent: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return responseError(resp)
	}
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("agent: decode %s response: %w", path, err)
	}
	return nil
}
