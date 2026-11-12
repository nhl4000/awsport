package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/nhl4000/awsport/internal/agent"
	"github.com/nhl4000/awsport/internal/awsx"
	"github.com/nhl4000/awsport/internal/bugreport"
	"github.com/nhl4000/awsport/internal/config"
	"github.com/nhl4000/awsport/internal/core"
	"github.com/nhl4000/awsport/internal/doctor"
	"github.com/nhl4000/awsport/internal/ghfetch"
	"github.com/nhl4000/awsport/internal/open"
	"github.com/nhl4000/awsport/internal/state"
	"github.com/nhl4000/awsport/internal/tunnel"
)

func newBugCmd(version, commit string) *cobra.Command {
	var (
		title        string
		yes, browser bool
		dryRun       bool
	)
	cmd := &cobra.Command{
		Use:   "bug",
		Short: "Report a bug on GitHub, with redacted diagnostics attached",
		Long: "Ask what happened, collect diagnostics (awsport, OS and AWS CLI versions,\n" +
			"awsport doctor results, the last " + fmt.Sprint(bugreport.LogLines) + " agent log lines and the latest crash\n" +
			"log from the past week), redact them for a public issue, show the result\n" +
			"and, once confirmed, file it in " + bugreport.RepoOwner + "/" + bugreport.RepoName + ".\n\n" +
			"Redacted: AWS profile, resource, group, role and sso-session names (mapped to\n" +
			"profile-N, resource-N, ...), account IDs, ARNs, AWS endpoints, instance IDs,\n" +
			"the SSO start URL, access key IDs, emails, private IPs and your home directory.\n\n" +
			"With a GitHub token ($GITHUB_TOKEN, $GH_TOKEN or `gh auth login`) the issue is\n" +
			"created through the API; otherwise (or with --browser) a prefilled issue form\n" +
			"opens in the browser.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			out := cmd.OutOrStdout()
			in := bufio.NewReader(cmd.InOrStdin())
			what := ""
			if title == "" {
				var err error
				if title, what, err = askDescription(in, out); err != nil {
					return err
				}
			}
			fmt.Fprintln(cmd.ErrOrStderr(), "Collecting diagnostics (runs awsport doctor; checking profiles can take a few seconds)…")
			rep := bugreport.Build(collectBugInput(ctx, version, commit, title, what))
			fmt.Fprintf(out, "\n──────── Issue preview ────────\nTitle: %s\n\n%s──────── End of preview ────────\n\n", rep.Title, rep.Body())
			if dryRun {
				return nil
			}
			if !yes {
				ok, err := confirm(in, out, fmt.Sprintf("Submit this as a public GitHub issue on %s/%s? [y/N] ", bugreport.RepoOwner, bugreport.RepoName))
				if err != nil {
					return err
				}
				if !ok {
					fmt.Fprintln(out, "Not submitted.")
					return nil
				}
			}
			tok, _ := (&ghfetch.Client{}).Token(ctx)
			res := bugreport.Submitter{API: bugreport.Client{Token: tok}, OpenBrowser: open.Browser}.Submit(ctx, rep, browser)
			printOutcome(out, res)
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&title, "title", "", "issue title; skips the questions")
	f.BoolVarP(&yes, "yes", "y", false, "submit without asking for confirmation")
	f.BoolVar(&browser, "browser", false, "open the prefilled issue form in the browser instead of using the GitHub API")
	f.BoolVar(&dryRun, "dry-run", false, "print the redacted issue and exit")
	return cmd
}

// askDescription prompts for a one-line title and a multi-line description
// ended by an empty line or end of input.
func askDescription(in *bufio.Reader, out io.Writer) (title, what string, err error) {
	fmt.Fprint(out, "Title (one line): ")
	line, err := in.ReadString('\n')
	title = strings.TrimSpace(line)
	if title == "" {
		if err != nil && !errors.Is(err, io.EOF) {
			return "", "", err
		}
		return "", "", errors.New("no title given (pass --title to skip the questions)")
	}
	fmt.Fprintln(out, "What happened? What did you expect? (end with an empty line or Ctrl-D)")
	var lines []string
	for {
		line, err := in.ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return "", "", err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break // empty line, or end of input
		}
		lines = append(lines, line)
		if err != nil {
			break
		}
	}
	return title, strings.Join(lines, "\n"), nil
}

func confirm(in *bufio.Reader, out io.Writer, prompt string) (bool, error) {
	fmt.Fprint(out, prompt)
	line, err := in.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return false, err
	}
	if errors.Is(err, io.EOF) && line == "" {
		fmt.Fprintln(out)
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true, nil
	}
	return false, nil
}

// collectBugInput gathers the report material and the redactor built from
// this machine's real profile, resource, group and SSO names. Doctor's
// profile listing, discovery and agent query are wrapped to record the
// names they see.
func collectBugInput(ctx context.Context, version, commit, title, what string) (bugreport.Input, *bugreport.Redactor) {
	names := &bugreport.Names{}
	if ps, err := awsx.ListProfiles(); err == nil {
		for _, p := range ps {
			names.AddProfile(p)
		}
	}
	if cfg, err := config.Load(); err == nil {
		names.AddConfig(cfg)
	}
	if session, startURL, _, ok, err := awsx.FindSSOSession(); err == nil && ok {
		names.AddSSO(session, startURL)
	}
	if st, err := state.Open(); err == nil {
		names.AddState(st.Get())
	}
	addDiscoveryCacheNames(names)

	d := doctor.System(version, commit)
	listProfiles, discover, inspect := d.Profiles, d.Discover, d.Agent
	d.Profiles = func() ([]core.Profile, error) {
		ps, err := listProfiles()
		for _, p := range ps {
			names.AddProfile(p)
		}
		return ps, err
	}
	d.Discover = func(ctx context.Context, p core.Profile, region string) ([]core.Resource, error) {
		rs, err := discover(ctx, p, region)
		for _, r := range rs {
			names.AddResource(r)
		}
		return rs, err
	}
	d.Agent = func() (*agent.Info, []tunnel.Session, error) {
		info, sessions, err := inspect()
		for _, s := range sessions {
			names.AddSession(s)
		}
		return info, sessions, err
	}
	in := bugreport.Input{Title: title, What: what, Doctor: doctor.Run(ctx, d, doctor.Options{})}

	if d.AgentLog != "" {
		in.LogName = filepath.Base(d.AgentLog)
		if lines, err := bugreport.TailFile(d.AgentLog, bugreport.LogLines); err == nil {
			in.Log = lines
			names.AddLogLines(lines)
		} else if !errors.Is(err, os.ErrNotExist) {
			in.Log = []string{"(can't read: " + err.Error() + ")"}
		}
	}
	if dir, err := config.StateDir(); err == nil {
		in.CrashName, in.Crash, _ = bugreport.LatestCrash(dir, time.Now())
	}
	home, _ := os.UserHomeDir()
	return in, names.Redactor(home)
}

// discoveryCacheFile is where the app persists the last discovery per
// profile, in the state directory.
const discoveryCacheFile = "discovery-cache.json"

// addDiscoveryCacheNames records the resources of the persisted discovery
// cache, whatever their age: it covers every region, unlike the single
// region per profile doctor checks. A missing or unreadable cache adds
// nothing.
func addDiscoveryCacheNames(names *bugreport.Names) {
	dir, err := config.StateDir()
	if err != nil {
		return
	}
	raw, err := os.ReadFile(filepath.Join(dir, discoveryCacheFile))
	if err != nil {
		return
	}
	var entries map[string]struct {
		Resources []core.Resource `json:"resources"`
	}
	if json.Unmarshal(raw, &entries) != nil {
		return
	}
	for profile, e := range entries {
		names.AddProfileName(profile)
		for _, r := range e.Resources {
			names.AddResource(r)
		}
	}
}

func printOutcome(out io.Writer, res bugreport.Outcome) {
	if res.IssueURL != "" {
		fmt.Fprintf(out, "Created %s\nThank you for the report!\n", hyperlink(out, res.IssueURL))
		return
	}
	if res.APIErr != nil {
		fmt.Fprintf(out, "Couldn't create the issue through the GitHub API: %v.\n", res.APIErr)
	}
	if res.BrowserErr != nil {
		fmt.Fprintf(out, "Couldn't open a browser (%v). Open this link to file the issue:\n", res.BrowserErr)
	} else {
		fmt.Fprintln(out, "Opened the prefilled issue form in your browser; review it and click \"Create\". If it didn't open, use this link:")
	}
	fmt.Fprintln(out, hyperlink(out, res.FormURL))
}

// hyperlink wraps u in an OSC 8 hyperlink when out is a terminal.
func hyperlink(out io.Writer, u string) string {
	f, ok := out.(*os.File)
	if !ok || os.Getenv("TERM") == "dumb" {
		return u
	}
	if fi, err := f.Stat(); err != nil || fi.Mode()&os.ModeCharDevice == 0 {
		return u
	}
	return "\x1b]8;;" + u + "\x1b\\" + u + "\x1b]8;;\x1b\\"
}
