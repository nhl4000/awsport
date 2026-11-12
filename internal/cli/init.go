package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"reflect"
	"slices"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/nhl4000/awsport/internal/app"
	"github.com/nhl4000/awsport/internal/awsx"
	"github.com/nhl4000/awsport/internal/config"
	"github.com/nhl4000/awsport/internal/core"
)

func newInitCmd() *cobra.Command {
	var from, startURL, region, session string
	var refresh bool
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Configure awsport non-interactively",
		Long: "Configure awsport without the TUI.\n\n" +
			"  --from <source>                   import a team config (YAML) from:\n" +
			"                                      github:OWNER/REPO/PATH[@REF] (private repos use\n" +
			"                                      $GITHUB_TOKEN, $GH_TOKEN or `gh auth token`),\n" +
			"                                      a github.com file URL, any http(s) URL, or a file path\n" +
			"  --refresh                         re-import the team config from where it came from\n" +
			"  --start-url <url> --region <r>    set the IAM Identity Center (SSO) portal\n" +
			"  (no flags)                        re-import the team config if one was imported,\n" +
			"                                    else adopt the first [sso-session] in the AWS config file",
		Example: "  awsport init --from github:acme/platform/awsport.team.yaml@main\n" +
			"  awsport init --from https://github.com/acme/platform/blob/main/awsport.team.yaml\n" +
			"  awsport init --refresh",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			out := cmd.OutOrStdout()
			ssoFlags := startURL != "" || region != "" || session != ""
			changed := false
			if from != "" {
				team, err := config.ImportTeam(cmd.Context(), from)
				if err != nil {
					return err
				}
				fmt.Fprintf(out, "Imported team config from %s (%d groups, %d regions, %d presets)\n",
					from, len(team.Groups), len(team.Regions), len(team.Presets))
				changed = true
			} else if refresh || !ssoFlags {
				src, err := teamSource()
				if err != nil {
					return err
				}
				switch {
				case src != "":
					if err := refreshTeam(cmd.Context(), out, src); err != nil {
						return err
					}
					changed = true
				case refresh:
					return errors.New("no team config to refresh: import one first with `awsport init --from <source>`")
				}
			}
			if ssoFlags {
				if err := saveSSO(startURL, region, session); err != nil {
					return err
				}
				fmt.Fprintln(out, "Saved SSO settings to user config")
				changed = true
			}
			if !changed {
				adopted, err := adoptSSOSession()
				if err != nil {
					return err
				}
				if adopted != "" {
					fmt.Fprintf(out, "Using sso-session %q from %s\n", adopted, awsx.DisplayPath(awsx.ConfigFilePath()))
				} else {
					return errors.New("nothing to configure: pass --from or --start-url/--region, or run `awsport` for interactive onboarding")
				}
			}

			a, err := app.New()
			if err != nil {
				return err
			}
			defer a.Close()
			if a.Cfg.SSO.StartURL == "" || a.Cfg.SSO.Region == "" {
				fmt.Fprintln(out, "SSO is not configured yet; run `awsport init --start-url <url> --region <region>`")
				return nil
			}
			if err := a.EnsureSSOSession(); err != nil {
				return err
			}
			fmt.Fprintln(out, "Next: run `awsport login` to sign in and create profiles")
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&from, "from", "", "team config to import: github:OWNER/REPO/PATH[@REF], URL or file path")
	f.BoolVar(&refresh, "refresh", false, "re-import the team config from its recorded source")
	f.StringVar(&startURL, "start-url", "", "SSO start URL, e.g. https://d-0000000000.awsapps.com/start")
	f.StringVar(&region, "region", "", "SSO region, e.g. us-east-1")
	f.StringVar(&session, "session", "", "sso-session name to write into the AWS config file (default \"awsport\")")
	cmd.MarkFlagsMutuallyExclusive("from", "refresh")
	return cmd
}

// teamSource returns the recorded team config source, or "".
func teamSource() (string, error) {
	u, err := config.LoadUser()
	if err != nil {
		return "", fmt.Errorf("load user config: %w", err)
	}
	return u.TeamSource, nil
}

// refreshTeam re-imports the team config from src and reports what changed.
func refreshTeam(ctx context.Context, out io.Writer, src string) error {
	// A missing or broken old team.yaml just means there is nothing to compare.
	before, _ := config.LoadTeam()
	if before == nil {
		before = &config.Config{}
	}
	after, err := config.ImportTeam(ctx, src)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "Re-imported team config from %s\n", src)
	writeTeamDiff(out, before, after)
	return nil
}

// writeTeamDiff prints group/region/preset counts before → after, naming
// what was added (+) or removed (-).
func writeTeamDiff(out io.Writer, before, after *config.Config) {
	if reflect.DeepEqual(before, after) {
		fmt.Fprintf(out, "  no changes (%d groups, %d regions, %d presets)\n",
			len(after.Groups), len(after.Regions), len(after.Presets))
		return
	}
	groupNames := func(gs []core.Group) []string {
		names := make([]string, len(gs))
		for i, g := range gs {
			names[i] = g.Name
		}
		return names
	}
	presetNames := func(ps []core.PortSpec) []string {
		names := make([]string, len(ps))
		for i, p := range ps {
			names[i] = p.Name + ":" + strconv.Itoa(p.Remote)
		}
		return names
	}
	listed := false
	for _, row := range []struct {
		label         string
		before, after []string
	}{
		{"groups", groupNames(before.Groups), groupNames(after.Groups)},
		{"regions", before.Regions, after.Regions},
		{"presets", presetNames(before.Presets), presetNames(after.Presets)},
	} {
		line := fmt.Sprintf("  %-8s %d → %d", row.label, len(row.before), len(row.after))
		var delta []string
		for _, n := range row.after {
			if !slices.Contains(row.before, n) {
				delta = append(delta, "+"+n)
			}
		}
		for _, n := range row.before {
			if !slices.Contains(row.after, n) {
				delta = append(delta, "-"+n)
			}
		}
		if len(delta) > 0 {
			line += "  (" + strings.Join(delta, ", ") + ")"
			listed = true
		}
		fmt.Fprintln(out, line)
	}
	if !listed {
		fmt.Fprintln(out, "  other settings changed (group members, SSO, shell, ...)")
	}
}

func saveSSO(startURL, region, session string) error {
	u, err := config.LoadUser()
	if err != nil {
		return fmt.Errorf("load user config: %w", err)
	}
	if startURL != "" {
		if err := validateStartURL(startURL); err != nil {
			return err
		}
		u.SSO.StartURL = startURL
	}
	if region != "" {
		u.SSO.Region = strings.TrimSpace(region)
	}
	if session != "" {
		u.SSO.Session = strings.TrimSpace(session)
	}
	if err := config.SaveUser(u); err != nil {
		return fmt.Errorf("save user config: %w", err)
	}
	return nil
}

// adoptSSOSession copies the first existing [sso-session] into the user
// config when no SSO is configured there. It returns the adopted name, or ""
// when there was nothing to adopt.
func adoptSSOSession() (string, error) {
	name, startURL, region, ok, err := awsx.FindSSOSession()
	if err != nil {
		return "", fmt.Errorf("read AWS config: %w", err)
	}
	if !ok {
		return "", nil
	}
	u, err := config.LoadUser()
	if err != nil {
		return "", fmt.Errorf("load user config: %w", err)
	}
	if u.SSO.StartURL != "" {
		return "", fmt.Errorf("SSO is already configured (%s); pass --start-url/--region to change it", u.SSO.StartURL)
	}
	u.SSO = config.SSO{StartURL: startURL, Region: region, Session: name}
	if err := config.SaveUser(u); err != nil {
		return "", fmt.Errorf("save user config: %w", err)
	}
	return name, nil
}

func validateStartURL(s string) error {
	u, err := url.Parse(s)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return fmt.Errorf("invalid start URL %q: want https://<id>.awsapps.com/start", s)
	}
	return nil
}
