package login

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/nhl4000/awsport/internal/open"
)

// Recorded `aws sso login` outputs (AWS CLI v2).
const (
	// --no-browser, device code flow (legacy profile or --use-device-code).
	sampleDeviceNoBrowser = "Browser will not be automatically opened.\n" +
		"Please visit the following URL:\n" +
		"\n" +
		"https://device.sso.us-east-1.amazonaws.com/\n" +
		"\n" +
		"Then enter the code:\n" +
		"\n" +
		"WDLC-QMXF\n" +
		"\n" +
		"Alternatively, you may visit the following URL which will autofill the code upon loading:\n" +
		"https://device.sso.us-east-1.amazonaws.com/?user_code=WDLC-QMXF\n"

	// --no-browser, authorization code (PKCE) flow for an sso-session.
	samplePKCENoBrowser = "Browser will not be automatically opened.\n" +
		"Please visit the following URL:\n" +
		"\n" +
		"https://oidc.us-east-1.amazonaws.com/authorize?response_type=code&client_id=abc123&redirect_uri=http%3A%2F%2F127.0.0.1%3A43517%2Foauth%2Fcallback&state=f00d&code_challenge_method=S256&scopes=sso%3Aaccount%3Aaccess&code_challenge=xyz\n" +
		"\n"

	// Without --no-browser, device code flow; the CLI opens the browser.
	sampleDeviceOpenBrowser = "Attempting to open your default browser.\r\n" +
		"If the browser does not open or you wish to use a different device to authorize this request, open the following URL:\r\n" +
		"\r\n" +
		"https://device.sso.eu-west-1.amazonaws.com/\r\n" +
		"\r\n" +
		"Then enter the code:\r\n" +
		"\r\n" +
		"KBTR-HNPZ\r\n"

	sampleUnknownOption = "\nusage: aws [options] <command> <subcommand> [<subcommand> ...] [parameters]\n" +
		"To see help text, you can run:\n\n  aws help\n  aws <command> help\n  aws <command> <subcommand> help\n\n" +
		"aws: error: Unknown options: --no-browser\n"
)

func scanAll(out string) linkScanner {
	var s linkScanner
	for _, l := range strings.Split(out, "\n") {
		s.feed(l)
	}
	return s
}

func TestLinkScanner(t *testing.T) {
	tests := []struct {
		name              string
		out               string
		wantURL, wantCode string
		wantLink          string
	}{
		{"device no-browser", sampleDeviceNoBrowser,
			"https://device.sso.us-east-1.amazonaws.com/", "WDLC-QMXF",
			"https://device.sso.us-east-1.amazonaws.com/?user_code=WDLC-QMXF"},
		{"pkce no-browser", samplePKCENoBrowser,
			"https://oidc.us-east-1.amazonaws.com/authorize?response_type=code&client_id=abc123&redirect_uri=http%3A%2F%2F127.0.0.1%3A43517%2Foauth%2Fcallback&state=f00d&code_challenge_method=S256&scopes=sso%3Aaccount%3Aaccess&code_challenge=xyz", "",
			"https://oidc.us-east-1.amazonaws.com/authorize?response_type=code&client_id=abc123&redirect_uri=http%3A%2F%2F127.0.0.1%3A43517%2Foauth%2Fcallback&state=f00d&code_challenge_method=S256&scopes=sso%3Aaccount%3Aaccess&code_challenge=xyz"},
		{"device opening browser, CRLF", sampleDeviceOpenBrowser,
			"https://device.sso.eu-west-1.amazonaws.com/", "KBTR-HNPZ",
			"https://device.sso.eu-west-1.amazonaws.com/"},
		{"complete link only", "https://device.sso.us-east-1.amazonaws.com/?user_code=ABCD-EFGH\n",
			"", "ABCD-EFGH", "https://device.sso.us-east-1.amazonaws.com/?user_code=ABCD-EFGH"},
		{"unrelated URL ignored", "See https://docs.aws.amazon.com/cli/ for help\nhttps://docs.aws.amazon.com/cli/\nSuccessfully logged into Start URL: https://d-0000000000.awsapps.com/start\n",
			"", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := scanAll(tt.out)
			if s.url != tt.wantURL || s.code != tt.wantCode || s.link() != tt.wantLink {
				t.Fatalf("got url=%q code=%q link=%q; want url=%q code=%q link=%q",
					s.url, s.code, s.link(), tt.wantURL, tt.wantCode, tt.wantLink)
			}
		})
	}
}

func TestWatcherAnnouncesDeviceLink(t *testing.T) {
	var out bytes.Buffer
	var opened []string
	w := &watcher{out: &out, settle: time.Hour, open: func(u string) error { opened = append(opened, u); return nil }}
	// Split writes mid-line, as pipe reads would.
	s := w.stream(&out)
	_, _ = s.Write([]byte(sampleDeviceNoBrowser[:40]))
	_, _ = s.Write([]byte(sampleDeviceNoBrowser[40:]))
	w.finish()

	link := "https://device.sso.us-east-1.amazonaws.com/?user_code=WDLC-QMXF"
	if len(opened) != 1 || opened[0] != link {
		t.Fatalf("opened %q; want [%q]", opened, link)
	}
	got := out.String()
	if !strings.HasPrefix(got, sampleDeviceNoBrowser) {
		t.Fatalf("CLI output not passed through first:\n%s", got)
	}
	for _, want := range []string{"Sign in to AWS with this link:\n\n  " + link + "\n", "Code: WDLC-QMXF", "Opening it in your browser"} {
		if !strings.Contains(got, want) {
			t.Errorf("output lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "\x1b]8;;") {
		t.Errorf("hyperlink escape written to a non-terminal:\n%q", got)
	}
	if !strings.Contains(w.transcript(), "Then enter the code") {
		t.Errorf("transcript lost CLI output")
	}
}

func TestWatcherHyperlinkAndOpenFailure(t *testing.T) {
	var out bytes.Buffer
	notFound := &exec.Error{Name: "xdg-open", Err: exec.ErrNotFound}
	w := &watcher{out: &out, hyperlinks: true, settle: time.Hour, open: func(string) error { return notFound }}
	_, _ = w.stream(&out).Write([]byte(sampleDeviceNoBrowser))
	w.finish()

	got := out.String()
	link := "https://device.sso.us-east-1.amazonaws.com/?user_code=WDLC-QMXF"
	if !strings.Contains(got, "\x1b]8;;"+link+"\x1b\\"+link+"\x1b]8;;\x1b\\") {
		t.Errorf("no OSC 8 hyperlink:\n%q", got)
	}
	want := "Couldn't open a browser automatically (xdg-open is not installed). Open the link above and enter the code.\n"
	if !strings.Contains(got, want) {
		t.Errorf("output lacks %q:\n%s", want, got)
	}
	if strings.Contains(got, "executable file not found") {
		t.Errorf("raw OS error shown:\n%s", got)
	}
}

func TestWatcherAnnouncesPKCELinkAfterSettling(t *testing.T) {
	var out bytes.Buffer
	opened := make(chan string, 1)
	w := &watcher{out: &out, settle: 10 * time.Millisecond, open: func(u string) error { opened <- u; return errors.New("boom") }}
	_, _ = w.stream(&out).Write([]byte(samplePKCENoBrowser))
	select {
	case u := <-opened:
		if !strings.HasPrefix(u, "https://oidc.us-east-1.amazonaws.com/authorize?") {
			t.Fatalf("opened %q", u)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("link never announced")
	}
	w.finish()
	got := w.transcript() // CLI output only; the block went to out
	if strings.Contains(got, "awsport") {
		t.Fatalf("transcript contains awsport's block: %q", got)
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if !strings.Contains(out.String(), "Couldn't open a browser automatically (boom). Open the link above.\n") {
		t.Fatalf("output:\n%s", out.String())
	}
}

func TestWatcherNoAnnouncementAfterFinish(t *testing.T) {
	var out bytes.Buffer
	w := &watcher{out: &out, settle: 20 * time.Millisecond, open: func(string) error { t.Error("browser opened after finish"); return nil }}
	_, _ = w.stream(&out).Write([]byte(samplePKCENoBrowser))
	w.finish()
	time.Sleep(60 * time.Millisecond)
	w.mu.Lock()
	defer w.mu.Unlock()
	if strings.Contains(out.String(), "awsport") {
		t.Fatalf("announced after finish:\n%s", out.String())
	}
}

func TestRejectsNoBrowser(t *testing.T) {
	if !rejectsNoBrowser(sampleUnknownOption) {
		t.Error("Unknown options: --no-browser not detected")
	}
	if rejectsNoBrowser(sampleDeviceNoBrowser) {
		t.Error("normal --no-browser output detected as rejection")
	}
	if rejectsNoBrowser("aws: error: Unknown options: --bogus\n") {
		t.Error("unrelated unknown option detected as --no-browser rejection")
	}
}

func TestDiagnose(t *testing.T) {
	same := func(p string) string { return p }
	linkedToWindows := func(p string) string {
		return strings.Replace(p, "/home/jane/.aws", "/mnt/c/Users/jane/.aws", 1)
	}
	tests := []struct {
		name    string
		out     string
		goos    string
		wsl     bool
		resolve func(string) string
		want    string
	}{
		{"cache not writable", "\n[Errno 13] Permission denied: '/home/jane/.aws/sso/cache/0f1e2d3c.json'\n", "linux", false, same,
			"awsport can't write /home/jane/.aws/sso/cache/0f1e2d3c.json: fix ownership with: sudo chown -R $(whoami) ~/.aws"},
		{"chmod not permitted", "[Errno 1] Operation not permitted: '/Users/jane/.aws/sso/cache'\n", "darwin", false, same,
			"awsport can't write /Users/jane/.aws/sso/cache: fix ownership with: sudo chown -R $(whoami) ~/.aws"},
		{"wsl windows drive", "[Errno 1] Operation not permitted: '/mnt/c/Users/jane/.aws/sso/cache/0f1e.json'\n", "linux", true, same,
			"awsport can't write /mnt/c/Users/jane/.aws/sso/cache/0f1e.json: it is on a Windows drive, which doesn't allow this from WSL; keep ~/.aws in your Linux home instead (a real directory, not a link to /mnt/…)"},
		{"wsl symlinked ~/.aws", "[Errno 1] Operation not permitted: '/home/jane/.aws/sso/cache'\n", "linux", true, linkedToWindows,
			"awsport can't write /home/jane/.aws/sso/cache (links to /mnt/c/Users/jane/.aws/sso/cache): it is on a Windows drive, which doesn't allow this from WSL; keep ~/.aws in your Linux home instead (a real directory, not a link to /mnt/…)"},
		{"/mnt path outside WSL", "[Errno 13] Permission denied: '/mnt/data/jane/.aws/sso/cache/x.json'\n", "linux", false, same,
			"awsport can't write /mnt/data/jane/.aws/sso/cache/x.json: fix ownership with: sudo chown -R $(whoami) ~/.aws"},
		{"windows path", `[Errno 13] Permission denied: 'C:\\Users\\jane\\.aws\\sso\\cache\\x.json'` + "\n", "windows", false, same,
			`awsport can't write C:\Users\jane\.aws\sso\cache\x.json: make sure your Windows user can write the .aws folder in your user profile`},
		{"denied outside ~/.aws", "[Errno 13] Permission denied: '/tmp/x'\n", "linux", false, same, ""},
		{"bad start URL", "\nAn error occurred (InvalidRequestException) when calling the StartDeviceAuthorization operation: \n", "linux", false, same,
			"the SSO start URL or region looks wrong; check sso_start_url and sso_region in your AWS config (or run `awsport init`)"},
		{"invalid grant", "\nAn error occurred (InvalidGrantException) when calling the CreateToken operation: invalid_grant\n", "linux", false, same,
			"the SSO start URL or region looks wrong; check sso_start_url and sso_region in your AWS config (or run `awsport init`)"},
		{"unknown", "\nThe config profile (x) could not be found\n", "linux", false, same, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := diagnose(tt.out, tt.goos, tt.wsl, tt.resolve); got != tt.want {
				t.Fatalf("diagnose =\n %q\nwant\n %q", got, tt.want)
			}
		})
	}
}

func TestShortReason(t *testing.T) {
	tests := []struct {
		err  error
		want string
	}{
		{fmt.Errorf("open https://x: %w", &exec.Error{Name: "/usr/bin/xdg-open", Err: exec.ErrNotFound}), "xdg-open is not installed"},
		{fmt.Errorf("open https://x: %w", &os.PathError{Op: "fork/exec", Path: "/usr/bin/wslview", Err: syscall.EPERM}), "permission denied"},
		{open.ErrNoDisplay, "no graphical display"},
		{fmt.Errorf("open https://x: %w", errors.New("launcher  crashed")), "launcher crashed"},
	}
	for _, tt := range tests {
		if got := shortReason(tt.err); got != tt.want {
			t.Errorf("shortReason(%v) = %q; want %q", tt.err, got, tt.want)
		}
	}
}

// fakeAWS writes an `aws` shell script that records its arguments and
// environment, then runs body.
func fakeAWS(t *testing.T, body string) (path, argsFile string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake CLI is a shell script")
	}
	dir := t.TempDir()
	argsFile = dir + "/args"
	path = dir + "/aws"
	script := "#!/bin/sh\necho \"$* PYTHONUNBUFFERED=$PYTHONUNBUFFERED\" >>" + argsFile + "\n" + body + "\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path, argsFile
}

func TestRunContextOpensLinkFromCLIOutput(t *testing.T) {
	aws, argsFile := fakeAWS(t, "cat <<'OUT'\n"+sampleDeviceNoBrowser+"OUT\necho 'Successfully logged into Start URL: https://d-0000000000.awsapps.com/start'")
	c := SSOSession(aws, "acme")
	var opened []string
	c.openBrowser = func(u string) error { opened = append(opened, u); return nil }
	var out, errOut bytes.Buffer
	if err := c.RunContext(t.Context(), nil, &out, &errOut); err != nil {
		t.Fatalf("RunContext: %v\n%s%s", err, out.String(), errOut.String())
	}
	if want := "https://device.sso.us-east-1.amazonaws.com/?user_code=WDLC-QMXF"; len(opened) != 1 || opened[0] != want {
		t.Fatalf("opened %q; want %q", opened, want)
	}
	args, _ := os.ReadFile(argsFile)
	if got := string(args); got != "sso login --no-browser --sso-session acme PYTHONUNBUFFERED=1\n" {
		t.Fatalf("CLI invoked as %q", got)
	}
	if !strings.Contains(out.String(), "Code: WDLC-QMXF") || !strings.Contains(out.String(), "Successfully logged into") {
		t.Fatalf("output:\n%s", out.String())
	}
}

func TestRunContextRetriesWithoutNoBrowser(t *testing.T) {
	aws, argsFile := fakeAWS(t, `case "$*" in *--no-browser*) printf '%s' '`+sampleUnknownOption+`' >&2; exit 252;; esac
echo 'Successfully logged into Start URL: https://d-0000000000.awsapps.com/start'`)
	c := Profile(aws, "example-dev")
	c.openBrowser = func(string) error { t.Error("awsport opened the browser although the CLI does"); return nil }
	var out, errOut bytes.Buffer
	if err := c.RunContext(t.Context(), nil, &out, &errOut); err != nil {
		t.Fatalf("RunContext: %v", err)
	}
	args, _ := os.ReadFile(argsFile)
	want := "sso login --no-browser --profile example-dev PYTHONUNBUFFERED=1\nsso login --profile example-dev PYTHONUNBUFFERED=1\n"
	if string(args) != want {
		t.Fatalf("invocations:\n%s\nwant:\n%s", args, want)
	}
}

func TestRunContextExplainsFailure(t *testing.T) {
	aws, _ := fakeAWS(t, `echo "[Errno 13] Permission denied: '/home/jane/.aws/sso/cache/0f1e.json'" >&2; exit 255`)
	c := SSOSession(aws, "acme")
	c.wsl = false
	err := c.RunContext(t.Context(), nil, &bytes.Buffer{}, &bytes.Buffer{})
	var le *Error
	if !errors.As(err, &le) || !strings.HasPrefix(err.Error(), "awsport can't write /home/jane/.aws/sso/cache/0f1e.json") {
		t.Fatalf("err = %v", err)
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("exit error not wrapped: %v", err)
	}

	aws, _ = fakeAWS(t, `echo "Error loading SSO Token: something odd" >&2; exit 255`)
	err = SSOSession(aws, "acme").RunContext(t.Context(), nil, &bytes.Buffer{}, &bytes.Buffer{})
	if err == nil || err.Error() != "aws sso login failed: Error loading SSO Token: something odd" {
		t.Fatalf("err = %v", err)
	}
}
