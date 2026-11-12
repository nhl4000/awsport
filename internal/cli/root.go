// Package cli implements the awsport command line.
package cli

import (
	"context"
	"fmt"
	"os"
	"runtime"

	"github.com/spf13/cobra"

	"github.com/nhl4000/awsport/internal/app"
	"github.com/nhl4000/awsport/internal/awsx"
	"github.com/nhl4000/awsport/internal/tui"
)

// Execute runs the CLI and returns the process exit code.
func Execute(version, commit string) int {
	root := NewRootCmd(version, commit)
	if err := root.ExecuteContext(context.Background()); err != nil {
		fmt.Fprintln(os.Stderr, "awsport:", err)
		return 1
	}
	return 0
}

// NewRootCmd builds the command tree. Without a subcommand awsport starts the
// TUI, which handles prerequisite checks and onboarding itself.
func NewRootCmd(version, commit string) *cobra.Command {
	app.Version = version
	root := &cobra.Command{
		Use:   "awsport",
		Short: "SSM port-forward tunnels and shells with stable local ports",
		Long: "awsport discovers EC2 and RDS resources in your AWS profiles and opens SSM\n" +
			"port-forwarding tunnels on stable, conflict-free local ports.\n\n" +
			"Unofficial — not affiliated with or endorsed by Amazon Web Services.",
		Version:       versionString(version, commit),
		SilenceUsage:  true,
		SilenceErrors: true,
		Args:          cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			a, err := app.New()
			if err != nil {
				return err
			}
			defer a.Close()
			return tui.Run(a)
		},
	}
	root.SetVersionTemplate("{{.Version}}\n")
	root.AddCommand(
		newInitCmd(),
		newLoginCmd(),
		newLsCmd(),
		newUpCmd(),
		newPsCmd(),
		newDownCmd(),
		newGroupsCmd(),
		newDoctorCmd(version, commit),
		newUpdateCmd(version),
		newVersionCmd(version, commit),
		newAgentCmd(version),
	)
	return root
}

func versionString(version, commit string) string {
	return fmt.Sprintf("awsport %s (%s) %s/%s", version, commit, runtime.GOOS, runtime.GOARCH)
}

func newVersionCmd(version, commit string) *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print version information",
		Args:  cobra.NoArgs,
		Run: func(cmd *cobra.Command, _ []string) {
			fmt.Fprintln(cmd.OutOrStdout(), versionString(version, commit))
		},
	}
}

// authHint appends a login hint to credential errors.
func authHint(err error, profile string) error {
	if err != nil && awsx.IsAuthError(err) {
		return fmt.Errorf("%w (run `awsport login %s`)", err, profile)
	}
	return err
}
