package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/nhl4000/awsport/internal/app"
	"github.com/nhl4000/awsport/internal/awsx"
)

func newLoginCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "login [profile]",
		Short: "Sign in via SSO (and sync profiles), or log in a single profile",
		Long: "Without arguments: sign in to the configured SSO session and write one\n" +
			"profile per account/role into the managed block of the AWS config file.\n" +
			"With a profile: run `aws sso login --profile <profile>`.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := app.New()
			if err != nil {
				return err
			}
			defer a.Close()
			out, errOut := cmd.OutOrStdout(), cmd.ErrOrStderr()

			if len(args) == 1 {
				p, err := a.Profile(args[0])
				if err != nil {
					return err
				}
				login := a.LoginCmd(p)
				if login == nil {
					return fmt.Errorf("profile %q uses %s credentials; nothing to log in", p.Name, p.Kind)
				}
				return runAttached(login, out, errOut)
			}

			if err := a.EnsureSSOSession(); err != nil {
				return err
			}
			if err := runAttached(awsx.SSOLoginCmd(a.AWSPath, a.SSOConfig().Session), out, errOut); err != nil {
				return err
			}
			n, err := a.SyncSSOProfiles(cmd.Context())
			if err != nil {
				return err
			}
			fmt.Fprintf(out, "Synced %d profiles into %s\n", n, awsx.ConfigFilePath())
			return nil
		},
	}
}
