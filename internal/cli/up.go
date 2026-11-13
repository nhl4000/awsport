package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/nhl4000/awsport/internal/app"
	"github.com/nhl4000/awsport/internal/core"
	"github.com/nhl4000/awsport/internal/open"
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
	var detach, noOpen bool
	cmd := &cobra.Command{
		Use:   "up <group | profile/name[:port]>...",
		Short: "Start tunnels (in the foreground; -d leaves them in the background)",
		Long: "Start tunnels and keep them running until interrupted. Ctrl-C stops the\n" +
			"tunnels this command started; tunnels that were already running stay up.\n" +
			"With --detach the tunnels keep running in the background after the command\n" +
			"exits; `awsport ps` lists them and `awsport down` stops them.\n\n" +
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
			openWeb := a.Cfg.OpenBrowserEnabled() && !noOpen
			return runUp(ctx, a, targets, detach, openWeb, cmd.OutOrStdout(), cmd.ErrOrStderr())
		},
	}
	cmd.Flags().BoolVarP(&detach, "detach", "d", false, "leave the tunnels running in the background and exit")
	cmd.Flags().BoolVar(&noOpen, "no-open", false, "don't open web tunnels in the browser once they are up")
	return cmd
}

func runUp(ctx context.Context, a *app.App, targets []upTarget, detach, openWeb bool, out, errOut io.Writer) error {
	before := activeKeys(a.Tunnels.Sessions())
	var specs []core.TunnelSpec
	failed := 0
	for _, t := range targets {
		if t.Group != "" {
			got, errs := a.StartGroup(ctx, t.Group)
			specs = append(specs, got...)
			for _, err := range errs {
				reportStartError(errOut, "", "", err)
				failed++
			}
			continue
		}
		spec, err := startTarget(ctx, a, t)
		if err != nil {
			reportStartError(errOut, t.Profile+"/"+t.Name+": ", t.Profile, err)
			failed++
			continue
		}
		specs = append(specs, spec)
	}
	own := ownKeys(before, specs)
	if len(specs) == 0 {
		return fmt.Errorf("no tunnels started (%d failed)", failed)
	}
	if err := writeSpecs(out, specs); err != nil {
		return err
	}
	opener := expectWeb(a, openWeb, own, specs, out)
	if detach {
		waitAndOpen(ctx, a, opener, out)
		fmt.Fprintln(out, "Tunnels keep running in the background: `awsport ps` lists them, `awsport down` stops them.")
		return nil
	}
	if len(own) == 0 {
		fmt.Fprintln(out, "All tunnels were already running; they stay up in the background.")
		return nil
	}
	if n := len(ownKeys(nil, specs)) - len(own); n > 0 {
		fmt.Fprintf(out, "%d tunnel(s) were already running and stay up after Ctrl-C.\n", n)
	}
	fmt.Fprintln(out, "Press Ctrl-C to stop.")

	mine := map[string]bool{}
	for _, k := range own {
		mine[k] = true
	}
	last := map[string]tunnel.Status{}
	events := a.Tunnels.Events()
	for {
		select {
		case <-ctx.Done():
			fmt.Fprintln(out, "Stopping tunnels…")
			return stopKeys(a.Tunnels, own)
		case ev, ok := <-events:
			if !ok {
				return errors.New("tunnel manager stopped")
			}
			if !mine[ev.Key] || last[ev.Key] == ev.Session.Status {
				continue
			}
			last[ev.Key] = ev.Session.Status
			printEvent(out, ev.Session)
			reportOpen(out, ev.Session.Spec.Label, opener, ev.Key, ev.Session.Status)
			if !anyActive(a.Tunnels.Get, own) {
				// Remove the failed sessions from the agent as well.
				_ = stopKeys(a.Tunnels, own)
				return errors.New("all tunnels have stopped")
			}
		}
	}
}

// detachOpenWait bounds how long `up -d` waits for web tunnels to come up
// before opening them.
const detachOpenWait = 20 * time.Second

// expectWeb registers the freshly started EC2 web tunnels (own keys only) to
// be opened in the browser once up, opening any that already are.
func expectWeb(a *app.App, enabled bool, own []string, specs []core.TunnelSpec, out io.Writer) *open.AutoOpener {
	opener := open.NewAutoOpener(open.Browser)
	if !enabled {
		return opener
	}
	byKey := map[string]core.TunnelSpec{}
	for _, s := range specs {
		byKey[s.Key] = s
	}
	for _, k := range own {
		spec := byKey[k]
		if !opener.Expect(spec) {
			continue
		}
		if s, ok := a.Tunnels.Get(k); ok {
			reportOpen(out, spec.Label, opener, k, s.Status)
		}
	}
	return opener
}

// waitAndOpen waits (bounded) for expected web tunnels to come up and opens
// them; used by `up -d`, which otherwise exits immediately.
func waitAndOpen(ctx context.Context, a *app.App, opener *open.AutoOpener, out io.Writer) {
	if opener.Pending() == 0 {
		return
	}
	timeout := time.NewTimer(detachOpenWait)
	defer timeout.Stop()
	events := a.Tunnels.Events()
	for opener.Pending() > 0 {
		select {
		case <-ctx.Done():
			return
		case <-timeout.C:
			fmt.Fprintln(out, "Some web tunnels weren't up yet; open them later with `awsport` (o) or the links above.")
			return
		case ev, ok := <-events:
			if !ok {
				return
			}
			reportOpen(out, ev.Session.Spec.Label, opener, ev.Key, ev.Session.Status)
		}
	}
}

// reportOpen feeds a status to opener and prints what happened when it
// tried to open a browser.
func reportOpen(out io.Writer, label string, opener *open.AutoOpener, key string, st tunnel.Status) {
	u, did, err := opener.Update(key, st == tunnel.Up, tunnel.IsActive(st))
	switch {
	case !did:
	case err != nil:
		fmt.Fprintf(out, "%s is up; couldn't open a browser (%v). Open %s yourself.\n", label, err, u)
	default:
		fmt.Fprintf(out, "Opened %s in your browser (%s).\n", u, label)
	}
}

// activeKeys returns the keys of the Starting, Up or Reconnecting sessions.
func activeKeys(sessions []tunnel.Session) map[string]bool {
	keys := map[string]bool{}
	for _, s := range sessions {
		if tunnel.IsActive(s.Status) {
			keys[s.Spec.Key] = true
		}
	}
	return keys
}

// ownKeys returns, deduplicated in order, the keys of specs that were not
// active before: the tunnels this invocation started.
func ownKeys(before map[string]bool, specs []core.TunnelSpec) []string {
	var own []string
	for _, s := range specs {
		if !before[s.Key] && !slices.Contains(own, s.Key) {
			own = append(own, s.Key)
		}
	}
	return own
}

// anyActive reports whether any of keys is Starting, Up or Reconnecting.
func anyActive(get func(key string) (tunnel.Session, bool), keys []string) bool {
	for _, k := range keys {
		if s, ok := get(k); ok && tunnel.IsActive(s.Status) {
			return true
		}
	}
	return false
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

// reportStartError prints a failed tunnel start: missing IAM permissions as
// calm warnings with the admin hint, anything else as an error line (with a
// login hint for profile when credentials expired).
func reportStartError(w io.Writer, prefix, profile string, err error) {
	perms, rest := splitPermissions(err)
	if rest != nil {
		if profile != "" {
			rest = authHint(rest, profile)
		}
		fmt.Fprintf(w, "error: %s%v\n", prefix, rest)
	}
	writePermissionWarnings(w, perms)
}
