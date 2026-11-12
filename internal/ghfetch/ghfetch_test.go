package ghfetch

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestParse(t *testing.T) {
	tests := []struct {
		name    string
		src     string
		want    File
		notGH   bool
		wantErr string
	}{
		{name: "short with ref", src: "github:acme/platform/awsport.team.yaml@main",
			want: File{Owner: "acme", Repo: "platform", Path: "awsport.team.yaml", Ref: "main"}},
		{name: "short nested path no ref", src: "github:acme/platform/config/team/awsport.yaml",
			want: File{Owner: "acme", Repo: "platform", Path: "config/team/awsport.yaml"}},
		{name: "short ref with slash", src: "github:acme/platform/a/b.yaml@feature/x",
			want: File{Owner: "acme", Repo: "platform", Path: "a/b.yaml", Ref: "feature/x"}},
		{name: "short tag ref", src: "github:acme/platform/b.yaml@v1.2.3",
			want: File{Owner: "acme", Repo: "platform", Path: "b.yaml", Ref: "v1.2.3"}},
		{name: "short missing path", src: "github:acme/platform", wantErr: "want github:OWNER/REPO/PATH"},
		{name: "short empty ref", src: "github:acme/platform/a.yaml@", wantErr: "want github:OWNER/REPO/PATH"},
		{name: "short empty segment", src: "github:acme//a.yaml", wantErr: "want github:OWNER/REPO/PATH"},
		{name: "short dot-dot", src: "github:acme/platform/../a.yaml", wantErr: "want github:OWNER/REPO/PATH"},
		{name: "web blob", src: "https://github.com/acme/platform/blob/main/deploy/awsport.team.yaml",
			want: File{Owner: "acme", Repo: "platform", Path: "deploy/awsport.team.yaml", Ref: "main"}},
		{name: "web raw with query", src: "https://github.com/acme/platform/raw/v2/a.yaml?raw=true",
			want: File{Owner: "acme", Repo: "platform", Path: "a.yaml", Ref: "v2"}},
		{name: "web repo page", src: "https://github.com/acme/platform", wantErr: "unsupported GitHub URL"},
		{name: "web tree", src: "https://github.com/acme/platform/tree/main/deploy", wantErr: "unsupported GitHub URL"},
		{name: "raw", src: "https://raw.githubusercontent.com/acme/platform/main/deploy/a.yaml",
			want: File{Owner: "acme", Repo: "platform", Path: "deploy/a.yaml", Ref: "main"}},
		{name: "raw refs heads", src: "https://raw.githubusercontent.com/acme/platform/refs/heads/main/a.yaml",
			want: File{Owner: "acme", Repo: "platform", Path: "a.yaml", Ref: "main"}},
		{name: "raw with token query", src: "https://raw.githubusercontent.com/acme/platform/abc123/a.yaml?token=XYZ",
			want: File{Owner: "acme", Repo: "platform", Path: "a.yaml", Ref: "abc123"}},
		{name: "raw too short", src: "https://raw.githubusercontent.com/acme/platform/main", wantErr: "unsupported GitHub URL"},
		{name: "other URL", src: "https://example.com/awsport.team.yaml", notGH: true},
		{name: "file path", src: "./awsport.team.yaml", notGH: true},
		{name: "windows path", src: `C:\team\awsport.yaml`, notGH: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok, err := Parse(tt.src)
			if ok == tt.notGH {
				t.Fatalf("ok = %v", ok)
			}
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Fatalf("got %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestFileString(t *testing.T) {
	if got := (File{Owner: "acme", Repo: "platform", Path: "a/b.yaml", Ref: "main"}).String(); got != "acme/platform/a/b.yaml@main" {
		t.Fatal(got)
	}
	if got := (File{Owner: "acme", Repo: "platform", Path: "b.yaml"}).String(); got != "acme/platform/b.yaml" {
		t.Fatal(got)
	}
}

// clearTokens removes ambient credentials so tests never use real ones.
func clearTokens(t *testing.T) {
	t.Helper()
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("GH_TOKEN", "")
	t.Setenv(EnvAPI, "")
}

func noGH(context.Context) string { return "" }

func TestOpenRequest(t *testing.T) {
	clearTokens(t)
	var got *http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r
		w.Header().Set("Content-Type", "application/vnd.github.raw; charset=utf-8")
		_, _ = io.WriteString(w, "regions: [us-east-1]\n")
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL + "/", GHToken: noGH}
	body, err := c.Open(context.Background(), File{Owner: "acme", Repo: "platform", Path: "deploy/team file.yaml", Ref: "feature/x"})
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(body)
	body.Close()
	if string(data) != "regions: [us-east-1]\n" {
		t.Fatalf("body = %q", data)
	}
	if got.URL.EscapedPath() != "/repos/acme/platform/contents/deploy/team%20file.yaml" {
		t.Errorf("path = %q", got.URL.EscapedPath())
	}
	if got.URL.Query().Get("ref") != "feature/x" {
		t.Errorf("ref = %q", got.URL.Query().Get("ref"))
	}
	if got.Header.Get("Accept") != "application/vnd.github.raw" || got.Header.Get("X-GitHub-Api-Version") != "2022-11-28" {
		t.Errorf("headers = %v", got.Header)
	}
	if a := got.Header.Get("Authorization"); a != "" {
		t.Errorf("Authorization = %q without any token", a)
	}
}

func TestOpenBaseFromEnv(t *testing.T) {
	clearTokens(t)
	hit := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hit = true
		_, _ = io.WriteString(w, "x")
	}))
	defer srv.Close()
	t.Setenv(EnvAPI, srv.URL)
	body, err := (&Client{GHToken: noGH}).Open(context.Background(), File{Owner: "a", Repo: "b", Path: "c"})
	if err != nil {
		t.Fatal(err)
	}
	body.Close()
	if !hit {
		t.Fatal("server not used")
	}
}

func TestTokenPrecedence(t *testing.T) {
	tests := []struct {
		name               string
		github, gh, ghExec string
		want               string
	}{
		{"GITHUB_TOKEN first", "t-github", "t-gh", "t-exec", "Bearer t-github"},
		{"GH_TOKEN second", "", "t-gh", "t-exec", "Bearer t-gh"},
		{"gh auth token last", "", "", "t-exec", "Bearer t-exec"},
		{"none", "", "", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clearTokens(t)
			t.Setenv("GITHUB_TOKEN", tt.github)
			t.Setenv("GH_TOKEN", tt.gh)
			var auth string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				auth = r.Header.Get("Authorization")
				_, _ = io.WriteString(w, "x")
			}))
			defer srv.Close()
			execCalled := false
			c := &Client{BaseURL: srv.URL, GHToken: func(context.Context) string {
				execCalled = true
				return tt.ghExec
			}}
			body, err := c.Open(context.Background(), File{Owner: "a", Repo: "b", Path: "c"})
			if err != nil {
				t.Fatal(err)
			}
			body.Close()
			if auth != tt.want {
				t.Fatalf("Authorization = %q, want %q", auth, tt.want)
			}
			if execCalled != (tt.github == "" && tt.gh == "") {
				t.Fatalf("gh exec called = %v", execCalled)
			}
		})
	}
}

func TestGHAuthToken(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script stand-in for gh")
	}
	dir := t.TempDir()
	t.Setenv("PATH", dir)
	if got := ghAuthToken(context.Background()); got != "" {
		t.Fatalf("without gh on PATH: %q", got)
	}
	script := "#!/bin/sh\n[ \"$1 $2\" = \"auth token\" ] && echo ' t-from-gh '\n"
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := ghAuthToken(context.Background()); got != "t-from-gh" {
		t.Fatalf("with gh on PATH: %q", got)
	}
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte("#!/bin/sh\necho 'not logged in' >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := ghAuthToken(context.Background()); got != "" {
		t.Fatalf("gh failing: %q", got)
	}
}

func TestOpenErrors(t *testing.T) {
	reset := time.Now().Add(30 * time.Minute).Unix()
	f := File{Owner: "acme", Repo: "platform", Path: "awsport.team.yaml", Ref: "main"}
	tests := []struct {
		name      string
		token     string // GITHUB_TOKEN
		ghToken   string // from the gh CLI
		status    int
		header    map[string]string
		login     string // GET /user response; "" means 401
		want      []string
		wantNot   []string
		wantLogin bool
	}{
		{name: "404 with token names account", token: "secret-tok", status: 404, login: "octo",
			want:      []string{"can't read acme/platform/awsport.team.yaml@main: not found, or your GitHub account has no access to acme/platform (logged in as octo?)"},
			wantLogin: true},
		{name: "403 with token, login lookup fails", token: "secret-tok", status: 403,
			want:      []string{"not found, or your GitHub account has no access to acme/platform"},
			wantNot:   []string{"logged in as"},
			wantLogin: true},
		{name: "404 without token", status: 404,
			want: []string{"can't read acme/platform/awsport.team.yaml@main:", "acme/platform is private", "`gh auth login`", "GITHUB_TOKEN"}},
		{name: "401 from env token", token: "secret-tok", status: 401,
			want: []string{"invalid or expired", "from GITHUB_TOKEN", "`gh auth refresh`"}},
		{name: "401 from gh", ghToken: "secret-gh", status: 401,
			want:    []string{"(from the gh CLI) is invalid or expired; run `gh auth refresh`"},
			wantNot: []string{"secret-gh"}},
		{name: "rate limited without token", status: 403,
			header: map[string]string{"X-RateLimit-Remaining": "0", "X-RateLimit-Reset": strconv.FormatInt(reset, 10)},
			want:   []string{"rate limit exceeded until " + time.Unix(reset, 0).Local().Format("15:04"), "`gh auth login`"}},
		{name: "rate limited with token", token: "secret-tok", status: 429,
			want:    []string{"rate limit exceeded", "try again later"},
			wantNot: []string{"no access"}},
		{name: "server error", status: 502, want: []string{"GitHub returned 502"}},
		{name: "directory", status: 200, header: map[string]string{"Content-Type": "application/json; charset=utf-8"},
			want: []string{"is a directory, not a file"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clearTokens(t)
			t.Setenv("GITHUB_TOKEN", tt.token)
			userHit := false
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/user" {
					userHit = true
					if tt.login == "" {
						w.WriteHeader(http.StatusUnauthorized)
						return
					}
					_, _ = io.WriteString(w, `{"login":"`+tt.login+`"}`)
					return
				}
				for k, v := range tt.header {
					w.Header().Set(k, v)
				}
				w.WriteHeader(tt.status)
				_, _ = io.WriteString(w, `{"message":"Not Found"}`)
			}))
			defer srv.Close()
			c := &Client{BaseURL: srv.URL, GHToken: func(context.Context) string { return tt.ghToken }}
			body, err := c.Open(context.Background(), f)
			if err == nil {
				body.Close()
				t.Fatal("expected error")
			}
			msg := err.Error()
			for _, w := range tt.want {
				if !strings.Contains(msg, w) {
					t.Errorf("error %q does not contain %q", msg, w)
				}
			}
			for _, w := range append(tt.wantNot, "secret-tok") {
				if strings.Contains(msg, w) {
					t.Errorf("error %q contains %q", msg, w)
				}
			}
			if userHit != tt.wantLogin {
				t.Errorf("GET /user called = %v, want %v", userHit, tt.wantLogin)
			}
		})
	}
}
