package login

import (
	"bytes"
	"net"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

const fullLink = "https://device.sso.us-east-1.amazonaws.com/?user_code=WDLC-QMXF"

// noFollow is an HTTP client that reports redirects instead of following them.
var noFollow = &http.Client{
	Timeout:       5 * time.Second,
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

func TestShortLinkRedirectsUntilStopped(t *testing.T) {
	short, stop, err := startShortLink(fullLink)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(short, "http://localhost:") {
		t.Fatalf("short link %q is not a localhost URL", short)
	}
	addr := "http://127.0.0.1:" + strings.TrimPrefix(short, "http://localhost:")

	resp, err := noFollow.Get(addr + "/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != fullLink {
		t.Fatalf("GET / = %d Location %q; want 302 to %q", resp.StatusCode, resp.Header.Get("Location"), fullLink)
	}
	if resp, err := noFollow.Get(addr + "/favicon.ico"); err != nil || resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET /favicon.ico = %v, %v; want 404", resp, err)
	}

	stop()
	if _, err := noFollow.Get(addr + "/"); err == nil {
		t.Fatal("short link still served after stop")
	}
}

func TestShortLinkFallsBackWhenPreferredPortIsBusy(t *testing.T) {
	busy, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(shortLinkPort)))
	if err != nil {
		t.Skipf("preferred port unavailable to occupy: %v", err)
	}
	defer busy.Close()
	short, stop, err := startShortLink(fullLink)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	if short == "http://localhost:"+strconv.Itoa(shortLinkPort) {
		t.Fatalf("short link reused the busy port: %s", short)
	}
}

func TestWatcherPrintsShortLinkAndStopsItOnFinish(t *testing.T) {
	var out bytes.Buffer
	stopped := false
	w := &watcher{out: &out, hyperlinks: true, settle: time.Hour,
		shorten: func(target string) (string, func(), error) {
			if target != fullLink {
				t.Errorf("shortened %q; want %q", target, fullLink)
			}
			return "http://localhost:17777", func() { stopped = true }, nil
		}}
	_, _ = w.stream(&out).Write([]byte(sampleDeviceNoBrowser))
	if stopped {
		t.Fatal("short link stopped before the login finished")
	}
	w.finish()
	if !stopped {
		t.Fatal("short link still running after the login finished")
	}
	got := out.String()
	for _, link := range []string{"http://localhost:17777", fullLink} {
		if !strings.Contains(got, "\x1b]8;;"+link+"\x1b\\"+link+"\x1b]8;;\x1b\\") {
			t.Errorf("%s is not printed as a clickable link:\n%q", link, got)
		}
	}
	if strings.Index(got, "http://localhost:17777") > strings.LastIndex(got, fullLink) {
		t.Errorf("short link should come before the full link:\n%s", got)
	}
}
