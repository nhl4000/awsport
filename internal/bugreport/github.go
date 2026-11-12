package bugreport

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/nhl4000/awsport/internal/ghfetch"
)

// Labels are applied to API-created issues when the user may set them.
var Labels = []string{"bug", "from-awsport"}

// APIError is a GitHub API failure to create the issue.
type APIError struct {
	Status  int    // HTTP status code
	Message string // GitHub's message, if any
}

func (e *APIError) Error() string {
	msg := fmt.Sprintf("GitHub returned %d %s", e.Status, http.StatusText(e.Status))
	if e.Message != "" {
		msg += ": " + e.Message
	}
	return msg
}

// Client creates issues through the GitHub REST API.
type Client struct {
	// BaseURL is the API base; default $AWSPORT_GITHUB_API, then
	// https://api.github.com.
	BaseURL string
	// HTTP is the client to use; default http.DefaultClient. Callers bound
	// requests with the context.
	HTTP  *http.Client
	Token string
}

// Create files the issue with Labels and returns its web URL. When GitHub
// rejects the labels (422: unknown labels; 403: no triage rights) it
// retries once without them.
func (c *Client) Create(ctx context.Context, title, body string) (string, error) {
	u, err := c.create(ctx, title, body, Labels)
	var ae *APIError
	if errors.As(err, &ae) && (ae.Status == http.StatusForbidden || ae.Status == http.StatusUnprocessableEntity) {
		u, err = c.create(ctx, title, body, nil)
	}
	return u, err
}

func (c *Client) create(ctx context.Context, title, body string, labels []string) (string, error) {
	payload, err := json.Marshal(struct {
		Title  string   `json:"title"`
		Body   string   `json:"body"`
		Labels []string `json:"labels,omitempty"`
	}{title, body, labels})
	if err != nil {
		return "", err
	}
	base := (&ghfetch.Client{BaseURL: c.BaseURL}).APIBase()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/repos/"+RepoOwner+"/"+RepoName+"/issues", bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "awsport")
	req.Header.Set("Authorization", "Bearer "+c.Token)
	hc := c.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var out struct {
		HTMLURL string `json:"html_url"`
		Message string `json:"message"`
	}
	_ = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out)
	if resp.StatusCode != http.StatusCreated {
		return "", &APIError{Status: resp.StatusCode, Message: strings.TrimSpace(out.Message)}
	}
	if out.HTMLURL == "" {
		return "", errors.New("GitHub created the issue but returned no URL")
	}
	return out.HTMLURL, nil
}
