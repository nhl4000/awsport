// Package login runs `aws sso login` for awsport. It passes the CLI's output
// through to the terminal while watching it: once the sign-in link (and
// device code) appear, it prints them as a clear, clickable block and opens
// the browser itself, because the AWS CLI's own launcher fails on some WSL and
// headless setups. Failed logins are translated into actionable messages.
package login

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/nhl4000/awsport/internal/deps"
	"github.com/nhl4000/awsport/internal/open"
)

const (
	// settleDelay is how long to wait after the sign-in link for a device
	// code before announcing the link without one (the PKCE flow has none).
	settleDelay = 300 * time.Millisecond
	// waitDelay bounds waiting for output pipes after the CLI exited (a
	// grandchild could keep them open).
	waitDelay = time.Second
	// maxTranscript is how much trailing CLI output is kept for diagnosis.
	maxTranscript = 64 << 10
)

// Cmd is one `aws sso login` run. It implements Bubble Tea's ExecCommand
// (Run, SetStdin, SetStdout, SetStderr) for use with tea.Exec; the CLI uses
// RunContext.
type Cmd struct {
	awsPath string
	target  []string // "--sso-session", name or "--profile", name

	stdin          io.Reader
	stdout, stderr io.Writer

	openBrowser func(string) error
	shortLink   func(target string) (short string, stop func(), err error) // nil: no short link
	wsl         bool
	settle      time.Duration
}

// SSOSession logs in to an [sso-session] (`aws sso login --sso-session`).
func SSOSession(awsPath, session string) *Cmd {
	return newCmd(awsPath, "--sso-session", session)
}

// Profile logs in to an SSO profile (`aws sso login --profile`).
func Profile(awsPath, profile string) *Cmd {
	return newCmd(awsPath, "--profile", profile)
}

func newCmd(awsPath, flag, name string) *Cmd {
	if awsPath == "" {
		awsPath = "aws"
	}
	return &Cmd{
		awsPath:     awsPath,
		target:      []string{flag, name},
		openBrowser: open.Browser,
		shortLink:   startShortLink,
		wsl:         deps.IsWSL(),
		settle:      settleDelay,
	}
}

// String is the command line, for display.
func (c *Cmd) String() string {
	return "aws sso login " + strings.Join(c.target, " ")
}

func (c *Cmd) SetStdin(r io.Reader)  { c.stdin = r }
func (c *Cmd) SetStdout(w io.Writer) { c.stdout = w }
func (c *Cmd) SetStderr(w io.Writer) { c.stderr = w }

// Run logs in using the streams set with SetStdin/SetStdout/SetStderr
// (os.Stdin/os.Stdout/os.Stderr when unset).
func (c *Cmd) Run() error {
	return c.RunContext(context.Background(), c.stdin, c.stdout, c.stderr)
}

// RunContext logs in, writing the CLI's output and awsport's sign-in block to
// the terminal. The returned error explains the failure in plain words.
func (c *Cmd) RunContext(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer) error {
	if stdin == nil {
		stdin = os.Stdin
	}
	if stdout == nil {
		stdout = os.Stdout
	}
	if stderr == nil {
		stderr = os.Stderr
	}
	fmt.Fprintf(stdout, "awsport: signing in to AWS (%s)\n\n", c)
	out, err := c.attempt(ctx, true, stdin, stdout, stderr)
	if err != nil && rejectsNoBrowser(out) {
		fmt.Fprintln(stdout, "\nawsport: this AWS CLI doesn't support --no-browser; trying again without it.")
		out, err = c.attempt(ctx, false, stdin, stdout, stderr)
	}
	if err != nil {
		return c.explain(ctx, out, err)
	}
	return nil
}

// attempt runs the CLI once and returns its (trailing) output. Output goes
// through pipes, scanned line by line; PYTHONUNBUFFERED makes the CLI's
// bundled Python write the sign-in link immediately instead of when it exits.
// With noBrowser set, awsport opens the browser itself.
func (c *Cmd) attempt(ctx context.Context, noBrowser bool, stdin io.Reader, stdout, stderr io.Writer) (string, error) {
	args := []string{"sso", "login"}
	if noBrowser {
		args = append(args, "--no-browser")
	}
	cmd := exec.CommandContext(ctx, c.awsPath, append(args, c.target...)...)
	cmd.Env = append(os.Environ(), "PYTHONUNBUFFERED=1")
	cmd.WaitDelay = waitDelay
	// The CLI never reads stdin during login; hand over the real terminal
	// only (a reader would keep Wait blocked on its copy goroutine).
	if f, ok := stdin.(*os.File); ok {
		cmd.Stdin = f
	}
	w := &watcher{out: stdout, hyperlinks: isTerminal(stdout), settle: c.settle, shorten: c.shortLink}
	if noBrowser {
		w.open = c.openBrowser
	}
	cmd.Stdout, cmd.Stderr = w.stream(stdout), w.stream(stderr)
	err := cmd.Run()
	w.finish()
	return w.transcript(), err
}

// rejectsNoBrowser reports whether the CLI refused --no-browser.
func rejectsNoBrowser(out string) bool {
	lower := strings.ToLower(out)
	return strings.Contains(lower, "no-browser") &&
		(strings.Contains(lower, "unknown option") || strings.Contains(lower, "unrecognized argument"))
}

// ---- output watching ----

// watcher copies CLI output to the terminal, keeps its tail and announces the
// sign-in link once it has been printed.
type watcher struct {
	mu         sync.Mutex
	out        io.Writer          // where the sign-in block goes
	hyperlinks bool               // wrap the link in an OSC 8 hyperlink
	open       func(string) error // nil: the CLI opens the browser itself
	shorten    func(string) (string, func(), error)
	settle     time.Duration

	log       []byte // trailing output, at most maxTranscript bytes
	scan      linkScanner
	timer     *time.Timer
	announced bool
	done      bool
	stopShort func() // shuts the short-link server down
}

// stream is one output stream of the CLI, copied to dst and scanned by line.
type stream struct {
	w       *watcher
	dst     io.Writer
	partial []byte // unterminated last line
}

func (w *watcher) stream(dst io.Writer) *stream { return &stream{w: w, dst: dst} }

func (s *stream) Write(p []byte) (int, error) {
	w := s.w
	w.mu.Lock()
	defer w.mu.Unlock()
	// Keep draining even if the terminal write fails; the CLI must not block.
	_, _ = s.dst.Write(p)
	w.log = append(w.log, p...)
	if over := len(w.log) - maxTranscript; over > 0 {
		w.log = append(w.log[:0], w.log[over:]...)
	}
	s.partial = append(s.partial, p...)
	for {
		i := bytes.IndexByte(s.partial, '\n')
		if i < 0 {
			break
		}
		line := string(s.partial[:i])
		s.partial = s.partial[i+1:]
		w.onLine(line)
	}
	return len(p), nil
}

func (w *watcher) onLine(line string) {
	if w.announced || !w.scan.feed(line) {
		return
	}
	if w.scan.complete != "" { // the device flow prints the complete link last
		w.announce()
		return
	}
	if w.timer == nil {
		w.timer = time.AfterFunc(w.settle, w.settled)
	} else {
		w.timer.Reset(w.settle)
	}
}

func (w *watcher) settled() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.done && !w.announced {
		w.announce()
	}
}

// finish stops pending announcements; nothing is written afterwards.
func (w *watcher) finish() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.done = true
	if w.timer != nil {
		w.timer.Stop()
	}
	if w.stopShort != nil {
		w.stopShort()
		w.stopShort = nil
	}
}

func (w *watcher) transcript() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return string(w.log)
}

// announce prints the sign-in block and opens the browser. Callers hold mu.
func (w *watcher) announce() {
	w.announced = true
	link, code := w.scan.link(), w.scan.code
	if link == "" {
		return
	}
	var b strings.Builder
	b.WriteString("\n──────── awsport ────────\n")
	b.WriteString("Sign in to AWS with this link:\n\n")
	if w.shorten != nil {
		if short, stop, err := w.shorten(link); err == nil {
			w.stopShort = stop
			b.WriteString("  " + w.hyperlink(short) + "   (short link, forwards to the full link below)\n")
		}
	}
	b.WriteString("  " + w.hyperlink(link) + "\n")
	if code != "" {
		b.WriteString("\nCode: " + code + "\n")
	}
	if w.open != nil {
		if err := w.open(link); err != nil {
			next := "Open the link above"
			if code != "" {
				next += " and enter the code"
			}
			fmt.Fprintf(&b, "\nCouldn't open a browser automatically (%s). %s.\n", shortReason(err), next)
		} else {
			b.WriteString("\nOpening it in your browser…\n")
		}
	}
	b.WriteString("─────────────────────────\n\n")
	_, _ = io.WriteString(w.out, b.String())
}

// hyperlink wraps u in an OSC 8 hyperlink when the output is a terminal.
func (w *watcher) hyperlink(u string) string {
	if !w.hyperlinks {
		return u
	}
	return "\x1b]8;;" + u + "\x1b\\" + u + "\x1b]8;;\x1b\\"
}

// shortReason is a brief, human cause for a failed browser launch.
func shortReason(err error) string {
	var ee *exec.Error
	switch {
	case errors.Is(err, open.ErrNoDisplay):
		return "no graphical display"
	case errors.As(err, &ee) && errors.Is(ee.Err, exec.ErrNotFound):
		return filepath.Base(ee.Name) + " is not installed"
	case errors.Is(err, fs.ErrPermission):
		return "permission denied"
	}
	for u := errors.Unwrap(err); u != nil; u = errors.Unwrap(u) {
		err = u
	}
	return strings.Join(strings.Fields(err.Error()), " ")
}

func isTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok || os.Getenv("TERM") == "dumb" {
		return false
	}
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

// linkScanner extracts the verification link and user code from
// `aws sso login` output, line by line.
type linkScanner struct {
	url      string // verification URL
	complete string // verification URL with ?user_code=, if printed
	code     string // device user code, if any

	wantURL  bool // after "... the following URL"
	wantCode bool // after "Then enter the code:"
}

var userCodeRe = regexp.MustCompile(`^[A-Za-z0-9]{4,}(?:-[A-Za-z0-9]{4,})*$`)

// feed consumes one output line and reports whether it was the link or code.
func (s *linkScanner) feed(line string) bool {
	l := strings.TrimSpace(line)
	lower := strings.ToLower(l)
	switch {
	case l == "":
		return false
	case strings.Contains(lower, "the following url"):
		s.wantURL = true
	case strings.Contains(lower, "enter the code"):
		s.wantCode = true
	case strings.HasPrefix(lower, "https://") && !strings.ContainsAny(l, " \t"):
		if !s.wantURL && !strings.Contains(lower, "/device") && !strings.Contains(lower, "user_code=") {
			return false
		}
		s.wantURL = false
		if !strings.Contains(lower, "user_code=") {
			if s.url == "" {
				s.url = l
			}
			return true
		}
		s.complete = l
		if s.code == "" {
			if u, err := url.Parse(l); err == nil {
				s.code = u.Query().Get("user_code")
			}
		}
		return true
	case s.wantCode && userCodeRe.MatchString(l):
		s.code, s.wantCode = l, false
		return true
	}
	return false
}

// link is the URL to open: the complete one (code prefilled) if printed.
func (s *linkScanner) link() string {
	if s.complete != "" {
		return s.complete
	}
	return s.url
}

// ---- failures ----

// Error is a failed login. Its message says what to do when the cause is
// recognized, otherwise it carries the CLI's last output line.
type Error struct {
	Msg string
	Err error // the underlying run error
}

func (e *Error) Error() string { return e.Msg }
func (e *Error) Unwrap() error { return e.Err }

// ErrCancelled means the user interrupted the login.
var ErrCancelled = errors.New("SSO login cancelled")

func (c *Cmd) explain(ctx context.Context, out string, err error) error {
	var exitErr *exec.ExitError
	switch {
	case ctx.Err() != nil:
		return &Error{Msg: ErrCancelled.Error(), Err: errors.Join(ErrCancelled, ctx.Err())}
	case errors.As(err, &exitErr) && (exitErr.ExitCode() == 130 || exitErr.ExitCode() == -1):
		return &Error{Msg: ErrCancelled.Error(), Err: errors.Join(ErrCancelled, err)}
	case errors.Is(err, exec.ErrNotFound) || errors.Is(err, fs.ErrNotExist):
		return &Error{Msg: fmt.Sprintf("the AWS CLI (%s) was not found; install AWS CLI v2", c.awsPath), Err: err}
	}
	if hint := diagnose(out, runtime.GOOS, c.wsl, resolvePath); hint != "" {
		return &Error{Msg: hint, Err: err}
	}
	if last := lastLine(out); last != "" {
		return &Error{Msg: "aws sso login failed: " + last, Err: err}
	}
	return &Error{Msg: "aws sso login failed: " + err.Error(), Err: err}
}

var (
	// Python's "[Errno 13] Permission denied: '/home/jane/.aws/sso/cache/x.json'".
	deniedPathRe   = regexp.MustCompile(`(?i)(?:permission denied|operation not permitted):\s*(?:'([^'\r\n]+)'|"([^"\r\n]+)")`)
	badSSOConfigRe = regexp.MustCompile(`InvalidRequestException|invalid_grant`)
)

// diagnose turns known `aws sso login` failures into one actionable sentence,
// or "" when the output is not recognized. resolve follows symlinks so a
// ~/.aws linked to a Windows drive is detected on WSL.
func diagnose(out, goos string, wsl bool, resolve func(string) string) string {
	if m := deniedPathRe.FindStringSubmatch(out); m != nil {
		p := m[1] + m[2]
		if goos == "windows" {
			p = strings.ReplaceAll(p, `\\`, `\`)
		}
		if underAWSDir(p) {
			return cantWrite(p, goos, wsl, resolve(p))
		}
	}
	if badSSOConfigRe.MatchString(out) {
		return "the SSO start URL or region looks wrong; check sso_start_url and sso_region in your AWS config (or run `awsport init`)"
	}
	return ""
}

func underAWSDir(p string) bool {
	p = strings.ReplaceAll(p, `\`, "/")
	return strings.Contains(p, "/.aws/") || strings.HasSuffix(p, "/.aws")
}

func cantWrite(p, goos string, wsl bool, resolved string) string {
	switch {
	case goos == "windows":
		return fmt.Sprintf("awsport can't write %s: make sure your Windows user can write the .aws folder in your user profile", p)
	case wsl && (strings.HasPrefix(p, "/mnt/") || strings.HasPrefix(resolved, "/mnt/")):
		where := p
		if resolved != p {
			where = fmt.Sprintf("%s (links to %s)", p, resolved)
		}
		return fmt.Sprintf("awsport can't write %s: it is on a Windows drive, which doesn't allow this from WSL; keep ~/.aws in your Linux home instead (a real directory, not a link to /mnt/…)", where)
	}
	return fmt.Sprintf("awsport can't write %s: fix ownership with: sudo chown -R $(whoami) ~/.aws", p)
}

// resolvePath follows symlinks in the longest existing prefix of p.
func resolvePath(p string) string {
	for q := p; ; q = filepath.Dir(q) {
		if r, err := filepath.EvalSymlinks(q); err == nil {
			rel, err := filepath.Rel(q, p)
			if err != nil {
				return p
			}
			return filepath.Join(r, rel)
		}
		if filepath.Dir(q) == q {
			return p
		}
	}
}

// lastLine is the last non-blank output line, whitespace-collapsed.
func lastLine(out string) string {
	lines := strings.Split(out, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if l := strings.Join(strings.Fields(lines[i]), " "); l != "" {
			return l
		}
	}
	return ""
}
