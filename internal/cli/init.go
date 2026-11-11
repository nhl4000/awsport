package cli

import (
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/spf13/cobra"

	"github.com/nhl4000/awsport/internal/app"
	"github.com/nhl4000/awsport/internal/awsx"
	"github.com/nhl4000/awsport/internal/config"
)

func newInitCmd() *cobra.Command {
	var from, startURL, region, session string
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Configure awsport non-interactively",
		Long: "Configure awsport without the TUI.\n\n" +
			"  --from <url|path>                 import a team config (YAML)\n" +
			"  --start-url <url> --region <r>    set the IAM Identity Center (SSO) portal\n" +
			"  (no flags)                        adopt the first [sso-session] in the AWS config file",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			out := cmd.OutOrStdout()
			changed := false
			if from != "" {
				team, err := config.ImportTeam(cmd.Context(), from)
				if err != nil {
					return fmt.Errorf("import team config: %w", err)
				}
				fmt.Fprintf(out, "Imported team config from %s (%d groups, %d regions, %d presets)\n",
					from, len(team.Groups), len(team.Regions), len(team.Presets))
				changed = true
			}
			if startURL != "" || region != "" || session != "" {
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
					fmt.Fprintf(out, "Using sso-session %q from %s\n", adopted, awsx.ConfigFilePath())
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
	f.StringVar(&from, "from", "", "team config URL or file path to import")
	f.StringVar(&startURL, "start-url", "", "SSO start URL, e.g. https://d-0000000000.awsapps.com/start")
	f.StringVar(&region, "region", "", "SSO region, e.g. us-east-1")
	f.StringVar(&session, "session", "", "sso-session name to write into the AWS config file (default \"awsport\")")
	return cmd
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
