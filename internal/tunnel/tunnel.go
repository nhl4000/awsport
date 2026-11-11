// Package tunnel supervises AWS SSM port-forwarding sessions. Each session
// runs `aws ssm start-session` as a child process under its own supervisor
// goroutine, which tracks readiness from the session-manager-plugin output,
// detects expired credentials and IAM access denials, and reconnects with
// backoff.
package tunnel

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nhl4000/awsport/internal/awsx"
	"github.com/nhl4000/awsport/internal/core"
)

// Status is the lifecycle state of a tunnel session.
type Status string

const (
	Starting     Status = "starting"
	Up           Status = "up"
	Reconnecting Status = "reconnecting"
	AuthExpired  Status = "auth-expired"
	Failed       Status = "failed"
	Stopped      Status = "stopped"
)

// Session is a snapshot of one supervised tunnel.
type Session struct {
	Spec     core.TunnelSpec `json:"spec"`
	Status   Status          `json:"status"`
	Err      string          `json:"err,omitempty"`      // last failure reason; empty while healthy
	Attempts int             `json:"attempts,omitempty"` // consecutive failed runs
	Since    time.Time       `json:"since"`              // when Status last changed
	LastLog  []string        `json:"lastLog,omitempty"`  // ring of the last 50 output lines (stdout+stderr)
	// Rev orders snapshots: every emitted change of any session in a Manager
	// gets the next value of a Manager-wide counter.
	Rev uint64 `json:"rev"`
}

// Event reports a session change. Session is a snapshot owned by the receiver.
type Event struct {
	Key     string  `json:"key"`
	Session Session `json:"session"`
}

// Controller drives tunnel sessions. It is implemented by *Manager (in
// process) and by the agent client (sessions owned by a background agent).
type Controller interface {
	Start(spec core.TunnelSpec) error
	Stop(key string) error
	StopAll() error
	Restart(key string) error
	RestartAuthExpired() ([]string, error)
	// Sessions returns snapshots sorted by Label (then Key). It never blocks
	// on the network.
	Sessions() []Session
	Get(key string) (Session, bool)
	Events() <-chan Event
	ActiveCount() int
}

var _ Controller = (*Manager)(nil)

// Options configures a Manager.
type Options struct {
	AWSPath     string          // default "aws" via exec.LookPath
	Reconnect   bool            // restart the session after it exits
	MaxFailures int             // consecutive failures before Failed; default 5
	Backoff     []time.Duration // default 1s,2s,5s,10s,30s (last repeats)
	// Command builds the process for one run of a session (test seam). It must
	// create the command with exec.CommandContext(ctx, ...). Default builds
	// `aws ssm start-session` from Args(spec).
	Command func(ctx context.Context, spec core.TunnelSpec) *exec.Cmd
}

const (
	logLines           = 50
	eventBuffer        = 256
	defaultMaxFailures = 5
	defaultKillAfter   = 3 * time.Second
	defaultResetAfter  = 60 * time.Second
	readyMarker        = "Waiting for connections"
	scanBufferMax      = 1 << 20
)

var defaultBackoff = []time.Duration{time.Second, 2 * time.Second, 5 * time.Second, 10 * time.Second, 30 * time.Second}

// authPatterns are lowercase substrings of AWS CLI output that mean the
// credentials are expired or invalid; reconnecting cannot fix those.
var authPatterns = []string{
	"expiredtoken",
	"invalidclienttokenid",
	"unrecognizedclientexception",
	"token has expired",
	"error loading sso token",
	"the sso session associated with this profile has expired",
}

// Manager owns all tunnel sessions of the process.
type Manager struct {
	opts       Options
	killAfter  time.Duration // grace period between interrupt and kill
	resetAfter time.Duration // Up time after which the failure counter resets
	events     chan Event

	mu       sync.Mutex
	sessions map[string]*entry
	rev      uint64 // last Rev handed out
}

type entry struct {
	spec   core.TunnelSpec // immutable
	sess   Session         // guarded by Manager.mu
	cancel context.CancelFunc
	done   chan struct{} // closed when the supervisor goroutine returns
}

// NewManager returns a Manager with defaults applied to o.
func NewManager(o Options) *Manager {
	if o.AWSPath == "" {
		o.AWSPath = "aws"
		if p, err := exec.LookPath("aws"); err == nil {
			o.AWSPath = p
		}
	}
	if o.MaxFailures <= 0 {
		o.MaxFailures = defaultMaxFailures
	}
	if len(o.Backoff) == 0 {
		o.Backoff = defaultBackoff
	}
	o.Backoff = slices.Clone(o.Backoff)
	if o.Command == nil {
		awsPath := o.AWSPath
		o.Command = func(ctx context.Context, spec core.TunnelSpec) *exec.Cmd {
			return exec.CommandContext(ctx, awsPath, Args(spec)...)
		}
	}
	return &Manager{
		opts:       o,
		killAfter:  defaultKillAfter,
		resetAfter: defaultResetAfter,
		events:     make(chan Event, eventBuffer),
		sessions:   make(map[string]*entry),
	}
}

// Args returns the aws CLI arguments for a port-forwarding session.
func Args(spec core.TunnelSpec) []string {
	params := map[string][]string{
		"portNumber":      {strconv.Itoa(spec.RemotePort)},
		"localPortNumber": {strconv.Itoa(spec.LocalPort)},
	}
	if spec.RemoteHost != "" {
		params["host"] = []string{spec.RemoteHost}
	}
	// Marshaling a map of string slices cannot fail.
	js, _ := json.Marshal(params)
	args := profileRegionArgs(spec.Profile, spec.Region)
	return append(args,
		"ssm", "start-session",
		"--target", spec.Target,
		"--document-name", spec.SSMDocument(),
		"--parameters", string(js),
	)
}

// ShellCmd builds an interactive SSM shell session running command on the
// instance, with stdio attached to the current terminal.
func ShellCmd(awsPath, profile, region, instanceID, command string) *exec.Cmd {
	js, _ := json.Marshal(map[string][]string{"command": {command}})
	args := profileRegionArgs(profile, region)
	args = append(args,
		"ssm", "start-session",
		"--target", instanceID,
		"--document-name", "AWS-StartInteractiveCommand",
		"--parameters", string(js),
	)
	cmd := exec.Command(awsPath, args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd
}

func profileRegionArgs(profile, region string) []string {
	var args []string
	if profile != "" {
		args = append(args, "--profile", profile)
	}
	if region != "" {
		args = append(args, "--region", region)
	}
	return args
}

// Start launches a supervised session for spec. It fails if a session with
// the same key is already active; an inactive (failed, auth-expired) session
// with that key is replaced.
func (m *Manager) Start(spec core.TunnelSpec) error {
	if spec.Key == "" {
		return errors.New("start tunnel: empty key")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if e, ok := m.sessions[spec.Key]; ok && IsActive(e.sess.Status) {
		return fmt.Errorf("start tunnel %q: already %s", spec.Key, e.sess.Status)
	}
	m.launchLocked(spec)
	return nil
}

// Stop gracefully stops the session and removes it. Unknown keys are ignored.
// The error is always nil; it exists to satisfy Controller.
func (m *Manager) Stop(key string) error {
	m.mu.Lock()
	e, ok := m.sessions[key]
	m.mu.Unlock()
	if ok {
		m.stopEntries([]*entry{e})
	}
	return nil
}

// StopAll gracefully stops every session concurrently: interrupt (terminate
// on Windows), wait, kill after 3s. The error is always nil.
func (m *Manager) StopAll() error {
	m.mu.Lock()
	entries := make([]*entry, 0, len(m.sessions))
	for _, e := range m.sessions {
		entries = append(entries, e)
	}
	m.mu.Unlock()
	m.stopEntries(entries)
	return nil
}

func (m *Manager) stopEntries(entries []*entry) {
	for _, e := range entries {
		e.cancel()
	}
	for _, e := range entries {
		<-e.done
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, e := range entries {
		if m.sessions[e.spec.Key] != e {
			continue
		}
		m.setStatusLocked(e, Stopped)
		m.emitLocked(e)
		delete(m.sessions, e.spec.Key)
	}
}

// Restart stops the session (if running) and starts it again with the same
// spec and local port, resetting its failure counter.
func (m *Manager) Restart(key string) error {
	m.mu.Lock()
	e, ok := m.sessions[key]
	m.mu.Unlock()
	if !ok {
		return fmt.Errorf("restart tunnel %q: not found", key)
	}
	e.cancel()
	<-e.done
	m.mu.Lock()
	defer m.mu.Unlock()
	if cur, ok := m.sessions[key]; ok && cur != e && IsActive(cur.sess.Status) {
		return fmt.Errorf("restart tunnel %q: already %s", key, cur.sess.Status)
	}
	m.launchLocked(e.spec)
	return nil
}

// RestartAuthExpired restarts every AuthExpired session (after re-login) and
// returns the restarted keys. The error is always nil.
func (m *Manager) RestartAuthExpired() ([]string, error) {
	m.mu.Lock()
	var keys []string
	for k, e := range m.sessions {
		if e.sess.Status == AuthExpired {
			keys = append(keys, k)
		}
	}
	m.mu.Unlock()
	slices.Sort(keys)
	restarted := keys[:0]
	for _, k := range keys {
		if m.Restart(k) == nil {
			restarted = append(restarted, k)
		}
	}
	return restarted, nil
}

// Sessions returns snapshots of all sessions sorted by Label (then Key).
func (m *Manager) Sessions() []Session {
	s, _ := m.Snapshot()
	return s
}

// Snapshot returns Sessions together with the Rev of the latest emitted
// change: every change with a higher Rev happened after the snapshot.
func (m *Manager) Snapshot() ([]Session, uint64) {
	m.mu.Lock()
	out := make([]Session, 0, len(m.sessions))
	for _, e := range m.sessions {
		out = append(out, snapshot(e))
	}
	rev := m.rev
	m.mu.Unlock()
	SortSessions(out)
	return out, rev
}

// Rev returns the Rev of the latest emitted change.
func (m *Manager) Rev() uint64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.rev
}

// SortSessions orders sessions by Label, then Key.
func SortSessions(s []Session) {
	slices.SortFunc(s, func(a, b Session) int {
		if c := strings.Compare(a.Spec.Label, b.Spec.Label); c != 0 {
			return c
		}
		return strings.Compare(a.Spec.Key, b.Spec.Key)
	})
}

// Get returns a snapshot of the session with key.
func (m *Manager) Get(key string) (Session, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.sessions[key]
	if !ok {
		return Session{}, false
	}
	return snapshot(e), true
}

// Events delivers session changes. The channel is buffered; when the consumer
// lags, the oldest pending events are dropped.
func (m *Manager) Events() <-chan Event { return m.events }

// ActiveCount counts sessions that are Starting, Up or Reconnecting.
func (m *Manager) ActiveCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, e := range m.sessions {
		if IsActive(e.sess.Status) {
			n++
		}
	}
	return n
}

// IsActive reports whether s is Starting, Up or Reconnecting.
func IsActive(s Status) bool {
	return s == Starting || s == Up || s == Reconnecting
}

func snapshot(e *entry) Session {
	s := e.sess
	s.LastLog = slices.Clone(s.LastLog)
	return s
}

func (m *Manager) launchLocked(spec core.TunnelSpec) {
	ctx, cancel := context.WithCancel(context.Background())
	e := &entry{
		spec:   spec,
		sess:   Session{Spec: spec, Status: Starting, Since: time.Now()},
		cancel: cancel,
		done:   make(chan struct{}),
	}
	m.sessions[spec.Key] = e
	m.emitLocked(e)
	go m.supervise(ctx, e)
}

func (m *Manager) setStatusLocked(e *entry, s Status) {
	if e.sess.Status != s {
		e.sess.Status = s
		e.sess.Since = time.Now()
	}
}

// emitLocked stamps the next Rev on e and sends a snapshot without blocking,
// dropping the oldest queued event when the buffer is full. Callers hold m.mu,
// which keeps events ordered.
func (m *Manager) emitLocked(e *entry) {
	m.rev++
	e.sess.Rev = m.rev
	ev := Event{Key: e.spec.Key, Session: snapshot(e)}
	for {
		select {
		case m.events <- ev:
			return
		default:
		}
		select {
		case <-m.events:
		default:
		}
	}
}

func (m *Manager) update(e *entry, fn func(s *Session)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	fn(&e.sess)
	m.emitLocked(e)
}

func (m *Manager) backoff(failures int) time.Duration {
	return m.opts.Backoff[min(failures, len(m.opts.Backoff))-1]
}

// supervise runs the session until it is cancelled, its credentials expire or
// it exhausts MaxFailures consecutive failures.
func (m *Manager) supervise(ctx context.Context, e *entry) {
	defer close(e.done)
	failures := 0
	for {
		upFor, authLine, denied, err := m.runOnce(ctx, e)
		if ctx.Err() != nil {
			return
		}
		if authLine != "" {
			m.update(e, func(s *Session) {
				m.setStatusLocked(e, AuthExpired)
				s.Err = authLine
			})
			return
		}
		if denied != "" {
			// Reconnecting cannot fix a missing IAM permission.
			m.update(e, func(s *Session) {
				m.setStatusLocked(e, Failed)
				s.Attempts = failures + 1
				s.Err = denied
			})
			return
		}
		if upFor > 0 && upFor >= m.resetAfter {
			failures = 0
		}
		failures++
		reason := exitReason(err)
		if !m.opts.Reconnect || failures >= m.opts.MaxFailures {
			m.update(e, func(s *Session) {
				m.setStatusLocked(e, Failed)
				s.Attempts = failures
				s.Err = reason
			})
			return
		}
		m.update(e, func(s *Session) {
			m.setStatusLocked(e, Reconnecting)
			s.Attempts = failures
			s.Err = reason
		})
		t := time.NewTimer(m.backoff(failures))
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
	}
}

func exitReason(err error) string {
	if err == nil {
		return "session ended"
	}
	return err.Error()
}

// deniedReason is the session error when IAM denied action on target.
func deniedReason(target, action string) string {
	if action == "" {
		action = "ssm:StartSession"
	}
	return fmt.Sprintf("not allowed to start an SSM session on %s (missing %s). Ask your AWS admin to allow it — see %s",
		target, action, awsx.IAMDocsURL)
}

// runOnce runs one process for the session and returns how long it was Up,
// the first output line matching an auth pattern, the denied reason when the
// output reports an IAM access denial, and the run error.
func (m *Manager) runOnce(ctx context.Context, e *entry) (upFor time.Duration, authLine, denied string, err error) {
	cmd := m.opts.Command(ctx, e.spec)
	if cmd == nil {
		return 0, "", "", errors.New("tunnel command builder returned nil")
	}
	if cmd.Cancel == nil {
		return 0, "", "", errors.New("tunnel command must be created with exec.CommandContext")
	}
	configureCmd(cmd, m.killAfter)
	pr, pw := io.Pipe()
	cmd.Stdin = nil
	cmd.Stdout = pw
	cmd.Stderr = pw
	if err := cmd.Start(); err != nil {
		pw.Close()
		pr.Close()
		return 0, "", "", fmt.Errorf("start %s: %w", filepath.Base(cmd.Path), err)
	}

	var upAt time.Time
	scanned := make(chan struct{})
	go func() {
		defer close(scanned)
		sc := bufio.NewScanner(pr)
		sc.Buffer(make([]byte, 0, 64*1024), scanBufferMax)
		for sc.Scan() {
			line := strings.TrimRight(sc.Text(), "\r")
			up := strings.Contains(line, readyMarker)
			if up && upAt.IsZero() {
				upAt = time.Now()
			}
			if authLine == "" && isAuthLine(line) {
				authLine = strings.TrimSpace(line)
			}
			if denied == "" {
				if action, _, ok := awsx.ParseAccessDenied(line); ok {
					denied = deniedReason(e.spec.Target, action)
				}
			}
			m.onLine(e, line, up)
		}
		// Keep draining after a scanner error so the child never blocks on a full pipe.
		_, _ = io.Copy(io.Discard, pr)
	}()

	err = cmd.Wait()
	pw.Close()
	<-scanned
	killGroup(cmd)
	if errors.Is(err, exec.ErrWaitDelay) {
		err = nil
	}
	if !upAt.IsZero() {
		upFor = time.Since(upAt)
	}
	return upFor, authLine, denied, err
}

func (m *Manager) onLine(e *entry, line string, up bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(e.sess.LastLog) < logLines {
		e.sess.LastLog = append(e.sess.LastLog, line)
	} else {
		copy(e.sess.LastLog, e.sess.LastLog[1:])
		e.sess.LastLog[len(e.sess.LastLog)-1] = line
	}
	if up && e.sess.Status != Up {
		m.setStatusLocked(e, Up)
		e.sess.Err = ""
	}
	m.emitLocked(e)
}

func isAuthLine(line string) bool {
	l := strings.ToLower(line)
	for _, p := range authPatterns {
		if strings.Contains(l, p) {
			return true
		}
	}
	return false
}
