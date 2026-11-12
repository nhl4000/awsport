// Package cli implements the awsport command line.
package cli

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"runtime/debug"
	"time"

	"github.com/spf13/cobra"

	"github.com/nhl4000/awsport/internal/app"
	"github.com/nhl4000/awsport/internal/awsx"
	"github.com/nhl4000/awsport/internal/bugreport"
	"github.com/nhl4000/awsport/internal/config"
	"github.com/nhl4000/awsport/internal/tui"
)

// Execute runs the CLI and returns the process exit code: 0, 1 on error,
// 2 after a panic (see crashed).
func Execute(version, commit string) (code int) {
	defer func() {
		if r := recover(); r != nil {
			code = crashed(r, debug.Stack(), version, commit)
		}
	}()
	root := NewRootCmd(version, commit)
	if err := root.ExecuteContext(context.Background()); err != nil {
		fmt.Fprintln(os.Stderr, "awsport:", err)
		return 1
	}
	return 0
}

// crashed saves a crash log for the panic r (a *bugreport.Panic carries the
// stack of the goroutine that panicked), tells the user how to report it
// and returns exit code 2.
func crashed(r any, stack []byte, version, commit string) int {
	if p, ok := r.(*bugreport.Panic); ok {
		r, stack = p.Value, p.Stack
	}
	dir, err := config.StateDir()
	if err == nil {
		var path string
		path, err = bugreport.WriteCrash(dir, bugreport.Crash{
			Value: r, Stack: stack, Version: version, Commit: commit, Args: os.Args, Time: time.Now(),
		})
		if err == nil {
			fmt.Fprintf(os.Stderr, "\nawsport: crash log saved to %s\n", awsx.DisplayPath(path))
		}
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "\npanic: %v\n\n%s\nawsport: %v\n", r, stack, err)
	}
	fmt.Fprintln(os.Stderr, "awsport crashed. Please report it with: awsport bug (the crash log is attached automatically)")
	return 2
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
		newBugCmd(version, commit),
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
