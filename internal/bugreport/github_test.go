package bugreport

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
)

type issueRequest struct {
	Title  string   `json:"title"`
	Body   string   `json:"body"`
	Labels []string `json:"labels"`
}

// fakeGitHub serves POST /repos/nhl4000/awsport/issues with respond and
// records the decoded requests.
func fakeGitHub(t *testing.T, respond func(w http.ResponseWriter, req issueRequest)) (*httptest.Server, *[]issueRequest) {
	t.Helper()
	var got []issueRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/repos/nhl4000/awsport/issues" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
			return
		}
		if auth := r.Header.Get("Authorization"); auth != "Bearer tok" {
			t.Errorf("Authorization = %q", auth)
		}
		var req issueRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode: %v", err)
		}
		got = append(got, req)
		respond(w, req)
	}))
	t.Cleanup(srv.Close)
	return srv, &got
}

func created(w http.ResponseWriter) {
	w.WriteHeader(http.StatusCreated)
	_, _ = w.Write([]byte(`{"html_url":"https://github.com/nhl4000/awsport/issues/42"}`))
}

func TestCreateWithLabels(t *testing.T) {
	srv, reqs := fakeGitHub(t, func(w http.ResponseWriter, _ issueRequest) { created(w) })
	c := Client{BaseURL: srv.URL, Token: "tok"}
	u, err := c.Create(context.Background(), "title", "body")
	if err != nil || u != "https://github.com/nhl4000/awsport/issues/42" {
		t.Fatalf("Create = %q, %v", u, err)
	}
	if len(*reqs) != 1 || !slices.Equal((*reqs)[0].Labels, Labels) || (*reqs)[0].Title != "title" || (*reqs)[0].Body != "body" {
		t.Errorf("requests %+v", *reqs)
	}
}

func TestCreateRetriesWithoutLabels(t *testing.T) {
	for _, status := range []int{http.StatusUnprocessableEntity, http.StatusForbidden} {
		srv, reqs := fakeGitHub(t, func(w http.ResponseWriter, req issueRequest) {
			if len(req.Labels) > 0 {
				w.WriteHeader(status)
				_, _ = w.Write([]byte(`{"message":"Validation Failed"}`))
				return
			}
			created(w)
		})
		u, err := (&Client{BaseURL: srv.URL, Token: "tok"}).Create(context.Background(), "t", "b")
		if err != nil || u == "" {
			t.Fatalf("status %d: Create = %q, %v", status, u, err)
		}
		if len(*reqs) != 2 || (*reqs)[1].Labels != nil {
			t.Errorf("status %d: requests %+v", status, *reqs)
		}
	}
}

func TestSubmitUnauthorizedOpensForm(t *testing.T) {
	srv, reqs := fakeGitHub(t, func(w http.ResponseWriter, _ issueRequest) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"message":"Bad credentials"}`))
	})
	var opened string
	s := Submitter{API: Client{BaseURL: srv.URL, Token: "tok"}, OpenBrowser: func(u string) error { opened = u; return nil }}
	rep := Report{Title: "t", What: "w", Doctor: "awsport doctor: 1 ok"}
	out := s.Submit(context.Background(), rep, false)

	var ae *APIError
	if !errors.As(out.APIErr, &ae) || ae.Status != http.StatusUnauthorized || !strings.Contains(ae.Error(), "Bad credentials") {
		t.Errorf("APIErr = %v", out.APIErr)
	}
	if len(*reqs) != 1 {
		t.Errorf("401 must not be retried: %d requests", len(*reqs))
	}
	if out.IssueURL != "" || out.FormURL == "" || opened != out.FormURL || out.FormURL != rep.FormURL() {
		t.Errorf("outcome %+v, opened %q", out, opened)
	}
}

func TestSubmitWithoutTokenOrForcedBrowser(t *testing.T) {
	srv, reqs := fakeGitHub(t, func(w http.ResponseWriter, _ issueRequest) { created(w) })
	rep := Report{Title: "t"}
	browserErr := errors.New("no display")
	noToken := Submitter{API: Client{BaseURL: srv.URL}, OpenBrowser: func(string) error { return browserErr }}.Submit(context.Background(), rep, false)
	if !errors.Is(noToken.APIErr, ErrNoToken) || noToken.FormURL == "" || noToken.BrowserErr != browserErr {
		t.Errorf("no token: %+v", noToken)
	}
	forced := Submitter{API: Client{BaseURL: srv.URL, Token: "tok"}, OpenBrowser: func(string) error { return nil }}.Submit(context.Background(), rep, true)
	if forced.APIErr != nil || forced.IssueURL != "" || forced.FormURL == "" {
		t.Errorf("--browser: %+v", forced)
	}
	if len(*reqs) != 0 {
		t.Errorf("API called %d times", len(*reqs))
	}
}

func TestSubmitSuccessSkipsBrowser(t *testing.T) {
	srv, _ := fakeGitHub(t, func(w http.ResponseWriter, _ issueRequest) { created(w) })
	s := Submitter{API: Client{BaseURL: srv.URL, Token: "tok"}, OpenBrowser: func(string) error {
		t.Error("browser opened")
		return nil
	}}
	out := s.Submit(context.Background(), Report{Title: "t"}, false)
	if out.IssueURL == "" || out.APIErr != nil || out.FormURL != "" {
		t.Errorf("outcome %+v", out)
	}
}

func TestClientUsesEnvAPIBase(t *testing.T) {
	srv, reqs := fakeGitHub(t, func(w http.ResponseWriter, _ issueRequest) { created(w) })
	t.Setenv("AWSPORT_GITHUB_API", srv.URL+"/")
	if _, err := (&Client{Token: "tok"}).Create(context.Background(), "t", "b"); err != nil || len(*reqs) != 1 {
		t.Errorf("Create via $AWSPORT_GITHUB_API: %v, %d requests", err, len(*reqs))
	}
}
