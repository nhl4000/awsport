package bugreport

import (
	"context"
	"errors"
)

// ErrNoToken means no GitHub token was found for the API path.
var ErrNoToken = errors.New("no GitHub token found (set GITHUB_TOKEN or run `gh auth login`)")

// Submitter files a report through the GitHub API, falling back to the
// prefilled issue form in the browser.
type Submitter struct {
	API         Client // API.Token "" skips the API
	OpenBrowser func(url string) error
}

// Outcome reports how Submit filed the issue.
type Outcome struct {
	IssueURL   string // the created issue; set when the API path succeeded
	APIErr     error  // why the API path failed or was skipped; nil with --browser
	FormURL    string // the prefilled issue form; set on the browser path
	BrowserErr error  // the browser could not be opened; FormURL must be opened by hand
}

// Submit creates the issue through the API unless forceBrowser is set or
// there is no token. On any API failure (401/403/404, network, ...) it opens
// the prefilled issue form instead, so the user can still file it.
func (s Submitter) Submit(ctx context.Context, r Report, forceBrowser bool) Outcome {
	var out Outcome
	if !forceBrowser {
		if s.API.Token == "" {
			out.APIErr = ErrNoToken
		} else {
			out.IssueURL, out.APIErr = s.API.Create(ctx, r.Title, r.Body())
			if out.APIErr == nil {
				return out
			}
		}
	}
	out.FormURL = r.FormURL()
	out.BrowserErr = s.OpenBrowser(out.FormURL)
	return out
}
