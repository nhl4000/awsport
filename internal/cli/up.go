package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/nhl4000/awsport/internal/app"
	"github.com/nhl4000/awsport/internal/core"
	"github.com/nhl4000/awsport/internal/tunnel"
)

// upTarget is one `awsport up` argument: a group name, or profile/name[:port].
type upTarget struct {
	Group   string
	Profile string
	Name    string
	Port    string // remote port number or port name; empty = default
}

// parseTarget parses "group" or "profile/name[:port]". The profile ends at the
// first "/", the port starts after the last ":".
func parseTarget(s string) (upTarget, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return upTarget{}, errors.New("empty target")
	}
	profile, rest, ok := strings.Cut(s, "/")
	if !ok {
		return upTarget{Group: s}, nil
	}
	if profile == "" || rest == "" {
		return upTarget{}, fmt.Errorf("invalid target %q: want profile/name[:port]", s)
	}
	t := upTarget{Profile: profile, Name: rest}
	if i := strings.LastIndex(rest, ":"); i >= 0 {
		t.Name, t.Port = rest[:i], rest[i+1:]
		if t.Name == "" || t.Port == "" {
			return upTarget{}, fmt.Errorf("invalid target %q: want profile/name[:port]", s)
		}
		if n, err := strconv.Atoi(t.Port); err == nil && (n <= 0 || n > 65535) {
			return upTarget{}, fmt.Errorf("invalid port %d in %q", n, s)
		}
	}
	return t, nil
}

// remotePort resolves a target port (number, port name or empty) for r.
func remotePort(r core.Resource, port string) (int, error) {
	if port == "" {
		switch {
		case r.Kind == core.KindRDS:
			return r.Port, nil
		case len(r.Ports) == 1:
			return r.Ports[0].Remote, nil
		default:
			return 0, fmt.Errorf("%s has several ports; pick one with %s/%s:<port> (available: %s)",
				r.Name, r.Profile, r.Name, portList(r.Ports))
		}
	}
	if n, err := strconv.Atoi(port); err == nil {
		return n, nil
	}
	for _, p := range r.Ports {
		if strings.EqualFold(p.Name, port) {
			return p.Remote, nil
		}
	}
	return 0, fmt.Errorf("%s has no port named %q (available: %s)", r.Name, port, portList(r.Ports))
}

func portList(ps []core.PortSpec) string {
	if len(ps) == 0 {
		return "none"
	}
	parts := make([]string, 0, len(ps))
	for _, p := range ps {
		parts = append(parts, p.String())
	}
	return strings.Join(parts, ", ")
}

func newUpCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "up <group | profile/name[:port]>...",
		Short: "Start tunnels in the foreground (Ctrl-C stops them)",
		Long: "Start tunnels and keep them running until interrupted.\n\n" +
			"Targets are group names or profile/name[:port], where name is an EC2 Name tag\n" +
			"or RDS identifier and port is a remote port number or port name. RDS targets\n" +
			"default to the database port.",
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			targets := make([]upTarget, 0, len(args))
			for _, arg := range args {
				t, err := parseTarget(arg)
				if err != nil {
					return err
				}
				targets = append(targets, t)
			}
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()

			a, err := app.New()
			if err != nil {
				return err
			}
			defer a.Close()
			return runUp(ctx, a, targets, cmd.OutOrStdout(), cmd.ErrOrStderr())
		},
	}
}

func runUp(ctx context.Context, a *app.App, targets []upTarget, out, errOut io.Writer) error {
	var specs []core.TunnelSpec
	failed := 0
	for _, t := range targets {
		if t.Group != "" {
			got, errs := a.StartGroup(ctx, t.Group)
			specs = append(specs, got...)
			for _, err := range errs {
				fmt.Fprintln(errOut, "error:", err)
				failed++
			}
			continue
		}
		spec, err := startTarget(ctx, a, t)
		if err != nil {
			fmt.Fprintf(errOut, "error: %s/%s: %v\n", t.Profile, t.Name, authHint(err, t.Profile))
			failed++
			continue
		}
		specs = append(specs, spec)
	}
	if len(specs) == 0 {
		return fmt.Errorf("no tunnels started (%d failed)", failed)
	}
	if err := writeSpecs(out, specs); err != nil {
		return err
	}
	fmt.Fprintln(out, "Press Ctrl-C to stop.")

	labels := map[string]string{}
	for _, s := range specs {
		labels[s.Key] = s.Label
	}
	last := map[string]tunnel.Status{}
	events := a.Tunnels.Events()
	for {
		select {
		case <-ctx.Done():
			fmt.Fprintln(out, "Stopping tunnels…")
			a.Close()
			return nil
		case ev, ok := <-events:
			if !ok {
				return errors.New("tunnel manager stopped")
			}
			if _, mine := labels[ev.Key]; !mine || last[ev.Key] == ev.Session.Status {
				continue
			}
			last[ev.Key] = ev.Session.Status
			printEvent(out, ev.Session)
			if a.Tunnels.ActiveCount() == 0 {
				a.Close()
				return errors.New("all tunnels have stopped")
			}
		}
	}
}

func startTarget(ctx context.Context, a *app.App, t upTarget) (core.TunnelSpec, error) {
	r, err := a.ResolveMember(ctx, core.GroupMember{Profile: t.Profile, Name: t.Name})
	if err != nil {
		return core.TunnelSpec{}, err
	}
	remote, err := remotePort(r, t.Port)
	if err != nil {
		return core.TunnelSpec{}, err
	}
	return a.Connect(ctx, r, remote)
}

func writeSpecs(w io.Writer, specs []core.TunnelSpec) error {
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "TUNNEL\tLOCAL\tPROFILE\tREGION")
	temporary := false
	for _, s := range specs {
		local := app.ConnString(s)
		if s.Temporary {
			local += " *"
			temporary = true
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", s.Label, local, s.Profile, s.Region)
	}
	if err := tw.Flush(); err != nil {
		return fmt.Errorf("write table: %w", err)
	}
	if temporary {
		fmt.Fprintln(w, "* temporary port: the saved port is busy; it is not remembered")
	}
	return nil
}

func printEvent(w io.Writer, s tunnel.Session) {
	line := fmt.Sprintf("%s  %-30s %s", time.Now().Format("15:04:05"), s.Spec.Label, s.Status)
	if s.Status == tunnel.Reconnecting && s.Attempts > 0 {
		line += fmt.Sprintf(" (attempt %d)", s.Attempts)
	}
	if s.Err != "" {
		line += ": " + s.Err
	}
	if s.Status == tunnel.AuthExpired {
		line += fmt.Sprintf(" (run `awsport login %s`)", s.Spec.Profile)
	}
	fmt.Fprintln(w, line)
}
