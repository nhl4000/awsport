package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/nhl4000/awsport/internal/app"
)

func newGroupsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "groups",
		Short: "List tunnel groups (team/user config and saved from the TUI)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			a, err := app.New()
			if err != nil {
				return err
			}
			defer a.Close()
			out := cmd.OutOrStdout()
			groups := a.Groups()
			if len(groups) == 0 {
				fmt.Fprintln(out, "No groups. Save one from the TUI or add `groups:` to the config file.")
				return nil
			}
			saved := map[string]bool{}
			for _, g := range a.State.Get().Groups {
				saved[g.Name] = true
			}
			for _, g := range groups {
				source := "config"
				if saved[g.Name] {
					source = "saved"
				}
				fmt.Fprintf(out, "%s (%s)\n", g.Name, source)
				for _, m := range g.Members {
					fmt.Fprintf(out, "  %s\n", app.MemberString(m))
				}
			}
			return nil
		},
	}
}
