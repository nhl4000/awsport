// Package ghfetch reads single files from GitHub repositories through the
// REST contents API, so private repositories work with the user's token.
package ghfetch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

const (
	// DefaultAPI is the GitHub REST API base URL.
	DefaultAPI = "https://api.github.com"
	// EnvAPI overrides the API base URL (for manual testing).
	EnvAPI = "AWSPORT_GITHUB_API"

	shortPrefix = "github:"
	apiVersion  = "2022-11-28"
	ghTimeout   = 5 * time.Second
	// sourceGH names the gh CLI as token source in messages.
	sourceGH = "the gh CLI"
)

// File identifies a file in a GitHub repository; an empty Ref means the
// repository's default branch.
type File struct {
	Owner, Repo, Path, Ref string
}

// String renders f as OWNER/REPO/PATH[@REF].
func (f File) String() string {
	s := f.Owner + "/" + f.Repo + "/" + f.Path
	if f.Ref != "" {
		s += "@" + f.Ref
	}
	return s
}

// Parse reports whether src names a file on GitHub and, if so, which one.
// Recognized forms:
//
//	github:OWNER/REPO/PATH[@REF]
//	https://github.com/OWNER/REPO/blob/REF/PATH (or /raw/)
//	https://raw.githubusercontent.com/OWNER/REPO/REF/PATH
//
// A malformed source in one of these forms yields ok=true and an error.
func Parse(src string) (f File, ok bool, err error) {
	if rest, found := strings.CutPrefix(src, shortPrefix); found {
		f, err = parseShort(src, rest)
		return f, true, err
	}
	u, perr := url.Parse(src)
	if perr != nil || (u.Scheme != "https" && u.Scheme != "http") {
		return File{}, false, nil
	}
	switch strings.ToLower(u.Hostname()) {
	case "github.com", "www.github.com":
		f, err = parseWeb(src, u.Path)
		return f, true, err
	case "raw.githubusercontent.com":
		f, err = parseRaw(src, u.Path)
		return f, true, err
	}
	return File{}, false, nil
}

func parseShort(src, rest string) (File, error) {
	bad := fmt.Errorf("invalid GitHub source %q: want github:OWNER/REPO/PATH[@REF], e.g. github:acme/platform/awsport.team.yaml@main", src)
	var ref string
	if i := strings.LastIndex(rest, "@"); i >= 0 {
		rest, ref = rest[:i], rest[i+1:]
		if ref == "" {
			return File{}, bad
		}
	}
	seg, ok := segments(rest)
	if !ok || len(seg) < 3 {
		return File{}, bad
	}
	return File{Owner: seg[0], Repo: seg[1], Path: strings.Join(seg[2:], "/"), Ref: ref}, nil
}

func parseWeb(src, path string) (File, error) {
	seg, ok := segments(path)
	if !ok || len(seg) < 5 || (seg[2] != "blob" && seg[2] != "raw") {
		return File{}, fmt.Errorf("unsupported GitHub URL %q: link to a file (https://github.com/OWNER/REPO/blob/REF/PATH) or use github:OWNER/REPO/PATH@REF", src)
	}
	return File{Owner: seg[0], Repo: seg[1], Ref: seg[3], Path: strings.Join(seg[4:], "/")}, nil
}

func parseRaw(src, path string) (File, error) {
	seg, ok := segments(path)
	if ok && len(seg) >= 6 && seg[2] == "refs" && (seg[3] == "heads" || seg[3] == "tags") {
		return File{Owner: seg[0], Repo: seg[1], Ref: seg[4], Path: strings.Join(seg[5:], "/")}, nil
	}
	if !ok || len(seg) < 4 {
		return File{}, fmt.Errorf("unsupported GitHub URL %q: want https://raw.githubusercontent.com/OWNER/REPO/REF/PATH or github:OWNER/REPO/PATH@REF", src)
	}
	return File{Owner: seg[0], Repo: seg[1], Ref: seg[2], Path: strings.Join(seg[3:], "/")}, nil
}

// segments splits a slash-separated path, rejecting empty, "." and ".."
// components (leading and trailing slashes are ignored).
func segments(p string) ([]string, bool) {
	seg := strings.Split(strings.Trim(p, "/"), "/")
	for _, s := range seg {
		if s == "" || s == "." || s == ".." {
			return nil, false
		}
	}
	return seg, true
}

// Client fetches files through the GitHub contents API. The zero value is
// ready to use.
type Client struct {
	// BaseURL overrides the API base; default $AWSPORT_GITHUB_API, then
	// DefaultAPI.
	BaseURL string
	// HTTP is the client to use; default http.DefaultClient. Callers bound
	// requests with the context.
	HTTP *http.Client
	// GHToken returns a token from the gh CLI, or "" if unavailable; default
	// runs `gh auth token` when gh is on PATH.
	GHToken func(ctx context.Context) string
}

// Open fetches f's raw content. Authentication uses the first token found
// in $GITHUB_TOKEN, $GH_TOKEN, then `gh auth token`. Failures are returned
// as plain-language errors that say how to fix access. The caller closes
// the returned body.
func (c *Client) Open(ctx context.Context, f File) (io.ReadCloser, error) {
	tok, src := c.Token(ctx)
	u := c.APIBase() + "/repos/" + url.PathEscape(f.Owner) + "/" + url.PathEscape(f.Repo) + "/contents/" + escapePath(f.Path)
	if f.Ref != "" {
		u += "?ref=" + url.QueryEscape(f.Ref)
	}
	resp, err := c.get(ctx, u, "application/vnd.github.raw", tok)
	if err != nil {
		return nil, fmt.Errorf("can't read %s: %w", f, err)
	}
	if resp.StatusCode == http.StatusOK {
		// The raw media type applies to files only; directories come back
		// as a JSON listing.
		if strings.HasPrefix(resp.Header.Get("Content-Type"), "application/json") {
			resp.Body.Close()
			return nil, fmt.Errorf("can't read %s: it is a directory, not a file", f)
		}
		return resp.Body, nil
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	resp.Body.Close()
	return nil, c.statusError(ctx, f, resp, tok, src)
}

func (c *Client) statusError(ctx context.Context, f File, resp *http.Response, tok, src string) error {
	if rateLimited(resp) {
		msg := fmt.Sprintf("can't read %s: GitHub API rate limit exceeded", f)
		if reset := rateReset(resp); !reset.IsZero() {
			msg += " until " + reset.Local().Format("15:04")
		}
		if tok == "" {
			return errors.New(msg + "; log in with `gh auth login` or set GITHUB_TOKEN for a higher limit")
		}
		return errors.New(msg + "; try again later")
	}
	repo := f.Owner + "/" + f.Repo
	switch resp.StatusCode {
	case http.StatusUnauthorized:
		fix := "run `gh auth refresh`"
		if src != sourceGH {
			fix = "set a new " + src + ", or unset it and run `gh auth refresh`"
		}
		return fmt.Errorf("can't read %s: your GitHub token (from %s) is invalid or expired; %s", f, src, fix)
	case http.StatusForbidden, http.StatusNotFound:
		if tok == "" {
			return fmt.Errorf("can't read %s: the file is missing or %s is private; log in with `gh auth login` or set GITHUB_TOKEN", f, repo)
		}
		msg := fmt.Sprintf("can't read %s: not found, or your GitHub account has no access to %s", f, repo)
		if login := c.login(ctx, tok); login != "" {
			msg += fmt.Sprintf(" (logged in as %s?)", login)
		}
		return errors.New(msg)
	}
	return fmt.Errorf("can't read %s: GitHub returned %s", f, resp.Status)
}

// rateLimited recognizes primary (remaining 0) and secondary (Retry-After)
// rate limits.
func rateLimited(resp *http.Response) bool {
	switch resp.StatusCode {
	case http.StatusTooManyRequests:
		return true
	case http.StatusForbidden:
		return resp.Header.Get("X-RateLimit-Remaining") == "0" || resp.Header.Get("Retry-After") != ""
	}
	return false
}

func rateReset(resp *http.Response) time.Time {
	if s, err := strconv.ParseInt(resp.Header.Get("X-RateLimit-Reset"), 10, 64); err == nil && s > 0 {
		return time.Unix(s, 0)
	}
	if s, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && s > 0 {
		return time.Now().Add(time.Duration(s) * time.Second)
	}
	return time.Time{}
}

// login returns the token owner's login, or "" on any failure.
func (c *Client) login(ctx context.Context, tok string) string {
	ctx, cancel := context.WithTimeout(ctx, ghTimeout)
	defer cancel()
	resp, err := c.get(ctx, c.APIBase()+"/user", "application/vnd.github+json", tok)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ""
	}
	var u struct {
		Login string `json:"login"`
	}
	if json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&u) != nil {
		return ""
	}
	return u.Login
}

func (c *Client) get(ctx context.Context, u, accept, tok string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", accept)
	req.Header.Set("X-GitHub-Api-Version", apiVersion)
	req.Header.Set("User-Agent", "awsport")
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	hc := c.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	return hc.Do(req)
}

// Token returns the first available GitHub token, from $GITHUB_TOKEN,
// $GH_TOKEN or `gh auth token` (5s timeout), and a description of its
// source; both are "" when there is none.
func (c *Client) Token(ctx context.Context) (tok, src string) {
	for _, env := range []string{"GITHUB_TOKEN", "GH_TOKEN"} {
		if v := strings.TrimSpace(os.Getenv(env)); v != "" {
			return v, env
		}
	}
	gh := c.GHToken
	if gh == nil {
		gh = ghAuthToken
	}
	if v := gh(ctx); v != "" {
		return v, sourceGH
	}
	return "", ""
}

// APIBase is the API base URL without a trailing slash: BaseURL, else
// $AWSPORT_GITHUB_API, else DefaultAPI.
func (c *Client) APIBase() string {
	b := c.BaseURL
	if b == "" {
		b = os.Getenv(EnvAPI)
	}
	if b == "" {
		b = DefaultAPI
	}
	return strings.TrimRight(b, "/")
}

// ghAuthToken runs `gh auth token` when gh is on PATH; "" on any failure.
func ghAuthToken(ctx context.Context) string {
	path, err := exec.LookPath("gh")
	if err != nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(ctx, ghTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, "auth", "token").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func escapePath(p string) string {
	seg := strings.Split(p, "/")
	for i, s := range seg {
		seg[i] = url.PathEscape(s)
	}
	return strings.Join(seg, "/")
}
