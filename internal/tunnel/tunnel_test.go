package tunnel

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nhl4000/awsport/internal/core"
)

const helperEnv = "GO_WANT_HELPER"

// TestHelperProcess is not a real test: the fake session process re-execs the
// test binary into it, selecting a behavior via $GO_WANT_HELPER.
func TestHelperProcess(t *testing.T) {
	mode := os.Getenv(helperEnv)
	if mode == "" {
		return
	}
	// Safety net: never outlive a crashed test run.
	go func() {
		time.Sleep(30 * time.Second)
		os.Exit(3)
	}()
	waitInterrupt := func() {
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, os.Interrupt)
		<-ch
		fmt.Println("interrupted")
		os.Exit(0)
	}
	switch mode {
	case "up":
		fmt.Println("Starting session with SessionId: example-0123456789abcdef0")
		fmt.Println("Port 15432 opened for sessionId example-0123456789abcdef0.")
		fmt.Println("Waiting for connections...")
		waitInterrupt()
	case "spam":
		for i := 1; i <= 120; i++ {
			fmt.Printf("line %d\n", i)
		}
		fmt.Println("Waiting for connections...")
		waitInterrupt()
	case "exit1":
		fmt.Fprintln(os.Stderr, "An error occurred (TargetNotConnected) when calling the StartSession operation: i-0123456789abcdef0 is not connected.")
		os.Exit(1)
	case "auth":
		fmt.Fprintln(os.Stderr, "An error occurred (ExpiredTokenException) when calling the StartSession operation: The security token included in the request is expired")
		os.Exit(255)
	case "sso":
		fmt.Fprintln(os.Stderr, "Error loading SSO Token: Token for example does not exist")
		os.Exit(255)
	case "upexit":
		fmt.Println("Waiting for connections...")
		time.Sleep(20 * time.Millisecond)
		os.Exit(0)
	case "stubborn":
		signal.Ignore(os.Interrupt)
		fmt.Println("Waiting for connections...")
		time.Sleep(time.Hour)
	case "family":
		signal.Ignore(os.Interrupt)
		child := exec.Command(os.Args[0], "-test.run=^TestHelperProcess$")
		child.Env = append(os.Environ(), helperEnv+"=stubborn")
		if err := child.Start(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(4)
		}
		fmt.Printf("child %d\n", child.Process.Pid)
		fmt.Println("Waiting for connections...")
		time.Sleep(time.Hour)
	default:
		fmt.Fprintf(os.Stderr, "unknown helper mode %q\n", mode)
		os.Exit(5)
	}
	os.Exit(0)
}

// fakeAWS hands out helper modes per run: run i uses modes[i], the last mode repeats.
type fakeAWS struct {
	modes []string
	calls atomic.Int32
}

func (f *fakeAWS) command(ctx context.Context, _ core.TunnelSpec) *exec.Cmd {
	i := int(f.calls.Add(1)) - 1
	mode := f.modes[min(i, len(f.modes)-1)]
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestHelperProcess$")
	// atexit_sleep_ms=0: race-enabled helpers otherwise linger 1s on exit.
	cmd.Env = append(os.Environ(), helperEnv+"="+mode, "GORACE=atexit_sleep_ms=0")
	return cmd
}

func newTestManager(t *testing.T, o Options, modes ...string) (*Manager, *fakeAWS) {
	t.Helper()
	f := &fakeAWS{modes: modes}
	o.AWSPath = "aws"
	o.Command = f.command
	if o.Backoff == nil {
		o.Backoff = []time.Duration{time.Millisecond}
	}
	m := NewManager(o)
	m.killAfter = 2 * time.Second
	t.Cleanup(m.StopAll)
	return m, f
}

func testSpec(key, label string) core.TunnelSpec {
	return core.TunnelSpec{
		Key: key, Label: label, Profile: "example", Region: "us-east-1",
		Target: "i-0123456789abcdef0", RemotePort: 5432, LocalPort: 15432,
	}
}

func waitStatus(t *testing.T, m *Manager, key string, want Status) Session {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		s, ok := m.Get(key)
		if ok && s.Status == want {
			return s
		}
		if time.Now().After(deadline) {
			t.Fatalf("session %q: want status %s, got %+v (present=%v)", key, want, s, ok)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestArgs(t *testing.T) {
	tests := []struct {
		name       string
		spec       core.TunnelSpec
		wantDoc    string
		wantParams map[string][]string
	}{
		{
			name:       "ec2 port forward",
			spec:       core.TunnelSpec{Profile: "example", Region: "us-east-1", Target: "i-0123456789abcdef0", RemotePort: 8025, LocalPort: 18025},
			wantDoc:    "AWS-StartPortForwardingSession",
			wantParams: map[string][]string{"portNumber": {"8025"}, "localPortNumber": {"18025"}},
		},
		{
			name:       "rds via bastion",
			spec:       core.TunnelSpec{Profile: "example", Region: "ca-central-1", Target: "i-0fedcba9876543210", RemoteHost: "db.example.internal", RemotePort: 5432, LocalPort: 15432},
			wantDoc:    "AWS-StartPortForwardingSessionToRemoteHost",
			wantParams: map[string][]string{"host": {"db.example.internal"}, "portNumber": {"5432"}, "localPortNumber": {"15432"}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			args := Args(tt.spec)
			wantPrefix := []string{
				"--profile", tt.spec.Profile, "--region", tt.spec.Region,
				"ssm", "start-session", "--target", tt.spec.Target,
				"--document-name", tt.wantDoc, "--parameters",
			}
			if len(args) != len(wantPrefix)+1 || !slices.Equal(args[:len(wantPrefix)], wantPrefix) {
				t.Fatalf("Args = %q, want prefix %q + params", args, wantPrefix)
			}
			var got map[string][]string
			if err := json.Unmarshal([]byte(args[len(args)-1]), &got); err != nil {
				t.Fatalf("parameters not JSON: %v", err)
			}
			if fmt.Sprint(got) != fmt.Sprint(tt.wantParams) {
				t.Fatalf("parameters = %v, want %v", got, tt.wantParams)
			}
		})
	}
}

func TestArgsOmitsEmptyProfileAndRegion(t *testing.T) {
	args := Args(core.TunnelSpec{Target: "i-0123456789abcdef0", RemotePort: 80, LocalPort: 10080})
	if args[0] != "ssm" {
		t.Fatalf("Args = %q, want to start with ssm", args)
	}
}

func TestShellCmd(t *testing.T) {
	cmd := ShellCmd("/usr/local/bin/aws", "example", "us-east-1", "i-0123456789abcdef0", "sudo su - ubuntu")
	want := []string{
		"/usr/local/bin/aws", "--profile", "example", "--region", "us-east-1",
		"ssm", "start-session", "--target", "i-0123456789abcdef0",
		"--document-name", "AWS-StartInteractiveCommand",
		"--parameters", `{"command":["sudo su - ubuntu"]}`,
	}
	if !slices.Equal(cmd.Args, want) {
		t.Fatalf("Args = %q, want %q", cmd.Args, want)
	}
	if cmd.Stdin != os.Stdin || cmd.Stdout != os.Stdout || cmd.Stderr != os.Stderr {
		t.Fatal("ShellCmd must attach stdio")
	}
}

func TestIsAuthLine(t *testing.T) {
	tests := []struct {
		line string
		want bool
	}{
		{"An error occurred (ExpiredTokenException) when calling the StartSession operation", true},
		{"An error occurred (InvalidClientTokenId) when calling the StartSession operation", true},
		{"An error occurred (UnrecognizedClientException) when calling the StartSession operation", true},
		{"Error when retrieving token from sso: Token has expired and refresh failed", true},
		{"Error loading SSO Token: Token for example does not exist", true},
		{"The SSO session associated with this profile has expired or is otherwise invalid.", true},
		{"An error occurred (TargetNotConnected) when calling the StartSession operation", false},
		{"Waiting for connections...", false},
	}
	for _, tt := range tests {
		if got := isAuthLine(tt.line); got != tt.want {
			t.Errorf("isAuthLine(%q) = %v, want %v", tt.line, got, tt.want)
		}
	}
}

func TestStartUpStop(t *testing.T) {
	m, _ := newTestManager(t, Options{Reconnect: true}, "up")
	spec := testSpec("example/us-east-1/i-0123456789abcdef0/5432", "app db")
	if err := m.Start(spec); err != nil {
		t.Fatal(err)
	}
	s := waitStatus(t, m, spec.Key, Up)
	if s.Spec != spec || !slices.ContainsFunc(s.LastLog, func(l string) bool { return strings.Contains(l, readyMarker) }) {
		t.Fatalf("unexpected session %+v", s)
	}
	if err := m.Start(spec); err == nil {
		t.Fatal("Start of an active key must fail")
	}
	if n := m.ActiveCount(); n != 1 {
		t.Fatalf("ActiveCount = %d, want 1", n)
	}
	m.Stop(spec.Key)
	if _, ok := m.Get(spec.Key); ok {
		t.Fatal("stopped session still present")
	}
	if n := m.ActiveCount(); n != 0 {
		t.Fatalf("ActiveCount = %d, want 0", n)
	}
	var statuses []Status
	for len(m.Events()) > 0 {
		ev := <-m.Events()
		if ev.Key != spec.Key {
			t.Fatalf("event for unexpected key %q", ev.Key)
		}
		if len(statuses) == 0 || statuses[len(statuses)-1] != ev.Session.Status {
			statuses = append(statuses, ev.Session.Status)
		}
	}
	if !slices.Equal(statuses, []Status{Starting, Up, Stopped}) {
		t.Fatalf("event statuses = %v, want [starting up stopped]", statuses)
	}
}

func TestAuthExpiredDoesNotReconnect(t *testing.T) {
	tests := []struct{ mode, wantErr string }{
		{"auth", "ExpiredTokenException"},
		{"sso", "Error loading SSO Token"},
	}
	for _, tt := range tests {
		t.Run(tt.mode, func(t *testing.T) {
			m, f := newTestManager(t, Options{Reconnect: true, MaxFailures: 5}, tt.mode)
			spec := testSpec("k", "l")
			if err := m.Start(spec); err != nil {
				t.Fatal(err)
			}
			s := waitStatus(t, m, spec.Key, AuthExpired)
			if !strings.Contains(s.Err, tt.wantErr) {
				t.Fatalf("Err = %q, want it to contain %q", s.Err, tt.wantErr)
			}
			time.Sleep(50 * time.Millisecond)
			if n := f.calls.Load(); n != 1 {
				t.Fatalf("command ran %d times, want 1", n)
			}
			if n := m.ActiveCount(); n != 0 {
				t.Fatalf("ActiveCount = %d, want 0", n)
			}
		})
	}
}

func TestFailureCap(t *testing.T) {
	tests := []struct {
		name      string
		reconnect bool
		max       int
		wantRuns  int
	}{
		{"reconnect until cap", true, 3, 3},
		{"reconnect disabled", false, 5, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, f := newTestManager(t, Options{Reconnect: tt.reconnect, MaxFailures: tt.max}, "exit1")
			spec := testSpec("k", "l")
			if err := m.Start(spec); err != nil {
				t.Fatal(err)
			}
			s := waitStatus(t, m, spec.Key, Failed)
			if s.Attempts != tt.wantRuns || int(f.calls.Load()) != tt.wantRuns {
				t.Fatalf("Attempts = %d, runs = %d, want %d", s.Attempts, f.calls.Load(), tt.wantRuns)
			}
			if !strings.Contains(s.Err, "exit status 1") || !strings.Contains(s.LastLog[len(s.LastLog)-1], "TargetNotConnected") {
				t.Fatalf("unexpected failure details: %+v", s)
			}
		})
	}
}

func TestReconnectThenUp(t *testing.T) {
	m, f := newTestManager(t, Options{Reconnect: true, MaxFailures: 5}, "exit1", "exit1", "up")
	spec := testSpec("k", "l")
	if err := m.Start(spec); err != nil {
		t.Fatal(err)
	}
	s := waitStatus(t, m, spec.Key, Up)
	if s.Attempts != 2 || f.calls.Load() != 3 || s.Err != "" {
		t.Fatalf("Attempts = %d, runs = %d, Err = %q; want 2, 3, empty", s.Attempts, f.calls.Load(), s.Err)
	}
}

func TestFailureCounterResetsAfterUptime(t *testing.T) {
	tests := []struct {
		name       string
		resetAfter time.Duration
		want       Status
	}{
		{"long uptime resets counter", time.Nanosecond, Up},
		{"short uptime keeps counting", time.Hour, Failed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, _ := newTestManager(t, Options{Reconnect: true, MaxFailures: 2},
				"exit1", "upexit", "upexit", "upexit", "up")
			m.resetAfter = tt.resetAfter
			spec := testSpec("k", "l")
			if err := m.Start(spec); err != nil {
				t.Fatal(err)
			}
			waitStatus(t, m, spec.Key, tt.want)
		})
	}
}

func TestStopDuringBackoff(t *testing.T) {
	m, _ := newTestManager(t, Options{Reconnect: true, Backoff: []time.Duration{time.Hour}}, "exit1")
	spec := testSpec("k", "l")
	if err := m.Start(spec); err != nil {
		t.Fatal(err)
	}
	waitStatus(t, m, spec.Key, Reconnecting)
	start := time.Now()
	m.Stop(spec.Key)
	if d := time.Since(start); d > time.Second {
		t.Fatalf("Stop during backoff took %v", d)
	}
	if _, ok := m.Get(spec.Key); ok {
		t.Fatal("stopped session still present")
	}
}

func TestStopAll(t *testing.T) {
	m, _ := newTestManager(t, Options{Reconnect: true}, "up")
	keys := []string{"a/1", "b/2", "c/3"}
	for _, k := range keys {
		if err := m.Start(testSpec(k, k)); err != nil {
			t.Fatal(err)
		}
	}
	for _, k := range keys {
		waitStatus(t, m, k, Up)
	}
	if n := m.ActiveCount(); n != 3 {
		t.Fatalf("ActiveCount = %d, want 3", n)
	}
	m.StopAll()
	if n, s := m.ActiveCount(), m.Sessions(); n != 0 || len(s) != 0 {
		t.Fatalf("after StopAll: ActiveCount = %d, sessions = %d", n, len(s))
	}
}

func TestRestartAuthExpired(t *testing.T) {
	m, _ := newTestManager(t, Options{Reconnect: true}, "auth", "up")
	spec := testSpec("k", "l")
	if err := m.Start(spec); err != nil {
		t.Fatal(err)
	}
	waitStatus(t, m, spec.Key, AuthExpired)
	if keys := m.RestartAuthExpired(); !slices.Equal(keys, []string{spec.Key}) {
		t.Fatalf("RestartAuthExpired = %v", keys)
	}
	s := waitStatus(t, m, spec.Key, Up)
	if s.Spec.LocalPort != spec.LocalPort || s.Attempts != 0 {
		t.Fatalf("restarted session %+v", s)
	}
	if keys := m.RestartAuthExpired(); len(keys) != 0 {
		t.Fatalf("second RestartAuthExpired = %v, want none", keys)
	}
	if err := m.Restart("missing"); err == nil {
		t.Fatal("Restart of unknown key must fail")
	}
}

func TestStartReplacesInactiveSession(t *testing.T) {
	m, _ := newTestManager(t, Options{}, "exit1", "up")
	spec := testSpec("k", "l")
	if err := m.Start(spec); err != nil {
		t.Fatal(err)
	}
	waitStatus(t, m, spec.Key, Failed)
	if err := m.Start(spec); err != nil {
		t.Fatalf("Start over failed session: %v", err)
	}
	waitStatus(t, m, spec.Key, Up)
}

func TestLastLogRing(t *testing.T) {
	m, _ := newTestManager(t, Options{}, "spam")
	spec := testSpec("k", "l")
	if err := m.Start(spec); err != nil {
		t.Fatal(err)
	}
	s := waitStatus(t, m, spec.Key, Up)
	if len(s.LastLog) != logLines || s.LastLog[0] != "line 72" || s.LastLog[logLines-1] != "Waiting for connections..." {
		t.Fatalf("LastLog len=%d first=%q last=%q", len(s.LastLog), s.LastLog[0], s.LastLog[len(s.LastLog)-1])
	}
}

func TestSessionsSortedByLabel(t *testing.T) {
	m, _ := newTestManager(t, Options{}, "up")
	for _, sp := range []core.TunnelSpec{testSpec("k1", "zeta"), testSpec("k2", "alpha"), testSpec("k3", "mid")} {
		if err := m.Start(sp); err != nil {
			t.Fatal(err)
		}
	}
	var labels []string
	for _, s := range m.Sessions() {
		labels = append(labels, s.Spec.Label)
	}
	if !slices.Equal(labels, []string{"alpha", "mid", "zeta"}) {
		t.Fatalf("labels = %v", labels)
	}
}

func TestEventsDropOldest(t *testing.T) {
	m := NewManager(Options{AWSPath: "aws"})
	e := &entry{spec: testSpec("k", "l")}
	m.mu.Lock()
	for i := range 300 {
		e.sess.Attempts = i
		m.emitLocked(e)
	}
	m.mu.Unlock()
	if n := len(m.Events()); n != eventBuffer {
		t.Fatalf("buffered events = %d, want %d", n, eventBuffer)
	}
	if ev := <-m.Events(); ev.Session.Attempts != 300-eventBuffer {
		t.Fatalf("oldest kept event = %d, want %d", ev.Session.Attempts, 300-eventBuffer)
	}
}

func TestCommandWithoutContextFails(t *testing.T) {
	m := NewManager(Options{AWSPath: "aws", Command: func(context.Context, core.TunnelSpec) *exec.Cmd {
		return exec.Command(os.Args[0], "-test.run=^TestHelperProcess$")
	}})
	spec := testSpec("k", "l")
	if err := m.Start(spec); err != nil {
		t.Fatal(err)
	}
	s := waitStatus(t, m, spec.Key, Failed)
	if !strings.Contains(s.Err, "CommandContext") {
		t.Fatalf("Err = %q", s.Err)
	}
}

func TestBackoffSchedule(t *testing.T) {
	m := NewManager(Options{AWSPath: "aws"})
	want := []time.Duration{time.Second, 2 * time.Second, 5 * time.Second, 10 * time.Second, 30 * time.Second, 30 * time.Second, 30 * time.Second}
	for i, w := range want {
		if got := m.backoff(i + 1); got != w {
			t.Errorf("backoff(%d) = %v, want %v", i+1, got, w)
		}
	}
}
