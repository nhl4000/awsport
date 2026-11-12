package cli

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/nhl4000/awsport/internal/doctor"
)

func newDoctorCmd(version, commit string) *cobra.Command {
	var (
		profiles []string
		report   string
		asJSON   bool
	)
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Diagnose the installation, SSO sign-in, profiles and permissions",
		Long: "Check awsport's prerequisites, files, SSO setup and sign-in, browser launch,\n" +
			"AWS profiles (credentials and discovery permissions) and the background agent.\n" +
			"Every problem comes with a fix. Exits 1 when any check fails.\n\n" +
			"--report writes a Markdown report with account IDs, emails, the SSO directory\n" +
			"ID, access key IDs and your home directory redacted, for sharing with an admin.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			res := doctor.Run(cmd.Context(), doctor.System(version, commit), doctor.Options{Profiles: profiles})
			out := cmd.OutOrStdout()
			if asJSON {
				enc := json.NewEncoder(out)
				enc.SetIndent("", "  ")
				if err := enc.Encode(res); err != nil {
					return fmt.Errorf("write JSON: %w", err)
				}
			} else if err := doctor.Render(out, res); err != nil {
				return err
			}
			if report != "" {
				home, _ := os.UserHomeDir()
				if err := os.WriteFile(report, []byte(doctor.Markdown(res, home)), 0o600); err != nil {
					return fmt.Errorf("write report: %w", err)
				}
				msgOut := out
				if asJSON {
					msgOut = cmd.ErrOrStderr() // keep stdout valid JSON
				}
				fmt.Fprintf(msgOut, "\nReport written to %s (redacted; review it before sharing)\n", report)
			}
			if n := res.Count(doctor.Fail); n > 0 {
				return fmt.Errorf("%d check(s) failed", n)
			}
			return nil
		},
	}
	f := cmd.Flags()
	f.StringSliceVar(&profiles, "profile", nil, "check only this profile (repeatable)")
	f.StringVar(&report, "report", "", "also write a redacted Markdown report to `FILE`")
	f.BoolVar(&asJSON, "json", false, "print the results as JSON")
	return cmd
}
