package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/signal"
	"slices"
	"strings"
	"sync"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/nhl4000/awsport/internal/agent"
	"github.com/nhl4000/awsport/internal/app"
	"github.com/nhl4000/awsport/internal/core"
	"github.com/nhl4000/awsport/internal/tunnel"
)

// lastLogWidth truncates the LAST LOG column of `ps`.
const lastLogWidth = 60

func newAgentCmd(version string) *cobra.Command {
	return &cobra.Command{
		Use:    "agent",
		Short:  "Run the background agent that owns the tunnels",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			return agent.Run(ctx, version)
		},
	}
}

type sessionJSON struct {
	Key       string        `json:"key"`
	Label     string        `json:"label"`
	Local     string        `json:"local"`
	Temporary bool          `json:"temporary,omitempty"`
	Status    tunnel.Status `json:"status"`
	Since     time.Time     `json:"since"`
	Profile   string        `json:"profile"`
	Region    string        `json:"region"`
	Error     string        `json:"error,omitempty"`
	LastLog   string        `json:"last_log,omitempty"`
}

func newPsCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "ps",
		Short: "List tunnels running in the background",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			a, err := app.New()
			if err != nil {
				return err
			}
			defer a.Close()
			sessions := a.Tunnels.Sessions()
			if asJSON {
				return writeSessionsJSON(cmd.OutOrStdout(), sessions)
			}
			if len(sessions) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "no tunnels running")
				return nil
			}
			return writeSessionsTable(cmd.OutOrStdout(), sessions, time.Now())
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	return cmd
}

func lastLogLine(s tunnel.Session) string {
	if len(s.LastLog) == 0 {
		return ""
	}
	return s.LastLog[len(s.LastLog)-1]
}

func writeSessionsJSON(w io.Writer, sessions []tunnel.Session) error {
	out := make([]sessionJSON, 0, len(sessions))
	for _, s := range sessions {
		out = append(out, sessionJSON{
			Key: s.Spec.Key, Label: s.Spec.Label, Local: app.ConnString(s.Spec), Temporary: s.Spec.Temporary,
			Status: s.Status, Since: s.Since, Profile: s.Spec.Profile, Region: s.Spec.Region,
			Error: s.Err, LastLog: lastLogLine(s),
		})
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(out); err != nil {
		return fmt.Errorf("write JSON: %w", err)
	}
	return nil
}

func writeSessionsTable(w io.Writer, sessions []tunnel.Session, now time.Time) error {
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "TUNNEL\tLOCAL\tSTATUS\tPROFILE\tUPTIME\tLAST LOG")
	for _, s := range sessions {
		local := app.ConnString(s.Spec)
		if s.Spec.Temporary {
			local += " *"
		}
		uptime := "-"
		if s.Status == tunnel.Up {
			uptime = now.Sub(s.Since).Round(time.Second).String()
		}
		last := lastLogLine(s)
		if r := []rune(last); len(r) > lastLogWidth {
			last = string(r[:lastLogWidth-1]) + "…"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", s.Spec.Label, local, s.Status, s.Spec.Profile, uptime, last)
	}
	if err := tw.Flush(); err != nil {
		return fmt.Errorf("write table: %w", err)
	}
	return nil
}

func newDownCmd() *cobra.Command {
	var all bool
	cmd := &cobra.Command{
		Use:   "down [--all] [target...]",
		Short: "Stop tunnels running in the background",
		Long: "Stop tunnels running in the background.\n\n" +
			"A target is a tunnel key, a group name or part of a tunnel label (case\n" +
			"insensitive); `awsport ps` lists them. --all stops every tunnel.",
		RunE: func(cmd *cobra.Command, args []string) error {
			switch {
			case all && len(args) > 0:
				return errors.New("--all takes no targets")
			case !all && len(args) == 0:
				return errors.New("name the tunnels to stop, or pass --all")
			}
			a, err := app.New()
			if err != nil {
				return err
			}
			defer a.Close()
			out := cmd.OutOrStdout()
			sessions := a.Tunnels.Sessions()
			if all {
				if len(sessions) == 0 {
					fmt.Fprintln(out, "no tunnels running")
					return nil
				}
				if err := a.StopAll(); err != nil {
					return err
				}
				fmt.Fprintf(out, "stopped %d tunnel(s)\n", len(sessions))
				return nil
			}
			keys, unmatched := matchSessions(sessions, args, a.Groups())
			if len(keys) == 0 {
				return fmt.Errorf("no running tunnel matches %s", strings.Join(args, ", "))
			}
			for _, t := range unmatched {
				fmt.Fprintf(cmd.ErrOrStderr(), "warning: no running tunnel matches %s\n", t)
			}
			err = stopKeys(a.Tunnels, keys)
			labels := map[string]string{}
			for _, s := range sessions {
				labels[s.Spec.Key] = s.Spec.Label
			}
			for _, k := range keys {
				fmt.Fprintf(out, "stopped %s\n", labels[k])
			}
			return err
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "stop every tunnel")
	return cmd
}

// matchSessions returns, sorted, the keys of the sessions any target
// matches, plus the targets that match none.
func matchSessions(sessions []tunnel.Session, targets []string, groups []core.Group) (keys, unmatched []string) {
	matched := map[string]bool{}
	for _, t := range targets {
		hits := matchTarget(sessions, t, groups)
		if len(hits) == 0 {
			unmatched = append(unmatched, t)
		}
		for _, k := range hits {
			matched[k] = true
		}
	}
	keys = slices.Sorted(maps.Keys(matched))
	return keys, unmatched
}

// matchTarget returns the keys of the sessions target selects: the session
// with that exact key, else the sessions of the group with that name, else
// the sessions whose label contains target (case insensitive).
func matchTarget(sessions []tunnel.Session, target string, groups []core.Group) []string {
	var keys []string
	for _, s := range sessions {
		if s.Spec.Key == target {
			return []string{target}
		}
	}
	if gi := slices.IndexFunc(groups, func(g core.Group) bool { return g.Name == target }); gi >= 0 {
		for _, s := range sessions {
			if slices.ContainsFunc(groups[gi].Members, func(m core.GroupMember) bool { return memberMatches(m, s.Spec) }) {
				keys = append(keys, s.Spec.Key)
			}
		}
		return keys
	}
	needle := strings.ToLower(target)
	for _, s := range sessions {
		if strings.Contains(strings.ToLower(s.Spec.Label), needle) {
			keys = append(keys, s.Spec.Key)
		}
	}
	return keys
}

// memberMatches reports whether spec is the tunnel of group member m. Tunnel
// labels are "<resource name> <port label>"; RDS tunnels always use the
// database port, whatever the member says.
func memberMatches(m core.GroupMember, spec core.TunnelSpec) bool {
	i := strings.LastIndex(spec.Label, " ")
	if i < 0 || spec.Label[:i] != m.Name || spec.Profile != m.Profile {
		return false
	}
	if m.Region != "" && spec.Region != m.Region {
		return false
	}
	return m.Remote == 0 || spec.RemoteHost != "" || spec.RemotePort == m.Remote
}

// stopKeys stops the sessions with keys concurrently.
func stopKeys(c tunnel.Controller, keys []string) error {
	errs := make([]error, len(keys))
	var wg sync.WaitGroup
	for i, k := range keys {
		wg.Go(func() { errs[i] = c.Stop(k) })
	}
	wg.Wait()
	return errors.Join(errs...)
}
