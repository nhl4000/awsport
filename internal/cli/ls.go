package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/nhl4000/awsport/internal/app"
	"github.com/nhl4000/awsport/internal/awsx"
	"github.com/nhl4000/awsport/internal/core"
)

// lsConcurrency bounds parallel profile discovery for `ls` without a profile.
const lsConcurrency = 4

type resourceJSON struct {
	Kind         core.ResourceKind `json:"kind"`
	Profile      string            `json:"profile"`
	Region       string            `json:"region"`
	ID           string            `json:"id"`
	Name         string            `json:"name"`
	State        string            `json:"state"`
	SSMOnline    bool              `json:"ssm_online,omitempty"`
	IsBastion    bool              `json:"is_bastion,omitempty"`
	Ports        []core.PortSpec   `json:"ports,omitempty"`
	ShellCommand string            `json:"shell_command,omitempty"`
	Host         string            `json:"host,omitempty"`
	Port         int               `json:"port,omitempty"`
	Engine       string            `json:"engine,omitempty"`
	Unreachable  string            `json:"unreachable,omitempty"`
}

func newLsCmd() *cobra.Command {
	var all, asJSON bool
	cmd := &cobra.Command{
		Use:   "ls [profile]",
		Short: "List connectable EC2 and RDS resources",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := app.New()
			if err != nil {
				return err
			}
			defer a.Close()

			var profiles []core.Profile
			if len(args) == 1 {
				p, err := a.Profile(args[0])
				if err != nil {
					return err
				}
				profiles = []core.Profile{p}
			} else if profiles, err = a.Profiles(); err != nil {
				return err
			}
			if len(profiles) == 0 {
				return fmt.Errorf("no AWS profiles found; run `awsport init` then `awsport login`")
			}

			resources, perms, errs := discoverAll(cmd.Context(), a, profiles)
			for _, err := range errs {
				fmt.Fprintln(cmd.ErrOrStderr(), "warning:", err)
			}
			if !all {
				kept := resources[:0]
				for _, r := range resources {
					if r.Unreachable == "" {
						kept = append(kept, r)
					}
				}
				resources = kept
			}
			sortResources(resources)
			if asJSON {
				err = writeResourcesJSON(cmd.OutOrStdout(), resources)
			} else {
				err = writeResourcesTable(cmd.OutOrStdout(), resources, all)
			}
			if err != nil {
				return err
			}
			writePermissionWarnings(cmd.ErrOrStderr(), perms)
			if len(errs) > 0 {
				return fmt.Errorf("discovery failed for %d of %d profiles", len(errs), len(profiles))
			}
			return nil
		},
	}
	cmd.Flags().BoolVarP(&all, "all", "a", false, "include unreachable resources with the reason")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	return cmd
}

// discoverAll discovers every profile concurrently. Calls denied by IAM are
// returned as perms; other failures (with a login hint for expired
// credentials) as errs.
func discoverAll(ctx context.Context, a *app.App, profiles []core.Profile) ([]core.Resource, []*awsx.PermissionError, []error) {
	var (
		mu        sync.Mutex
		wg        sync.WaitGroup
		resources []core.Resource
		perms     []*awsx.PermissionError
		errs      []error
	)
	sem := make(chan struct{}, lsConcurrency)
	for _, p := range profiles {
		wg.Add(1)
		go func(p core.Profile) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			res, err := a.Discover(ctx, p)
			denied, rest := splitPermissions(err)
			mu.Lock()
			defer mu.Unlock()
			resources = append(resources, res...)
			perms = append(perms, denied...)
			if rest != nil {
				errs = append(errs, authHint(rest, p.Name))
			}
		}(p)
	}
	wg.Wait()
	sort.Slice(errs, func(i, j int) bool { return errs[i].Error() < errs[j].Error() })
	sort.SliceStable(perms, func(i, j int) bool { return perms[i].Error() < perms[j].Error() })
	return resources, perms, errs
}

func sortResources(rs []core.Resource) {
	sort.Slice(rs, func(i, j int) bool {
		a, b := rs[i], rs[j]
		if a.Profile != b.Profile {
			return a.Profile < b.Profile
		}
		if a.Region != b.Region {
			return a.Region < b.Region
		}
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		if a.Name != b.Name {
			return a.Name < b.Name
		}
		return a.ID < b.ID
	})
}

func writeResourcesJSON(w io.Writer, rs []core.Resource) error {
	out := make([]resourceJSON, 0, len(rs))
	for _, r := range rs {
		out = append(out, resourceJSON{
			Kind: r.Kind, Profile: r.Profile, Region: r.Region, ID: r.ID, Name: r.Name, State: r.State,
			SSMOnline: r.SSMOnline, IsBastion: r.IsBastion, Ports: r.Ports, ShellCommand: r.ShellCommand,
			Host: r.Host, Port: r.Port, Engine: r.Engine, Unreachable: r.Unreachable,
		})
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(out); err != nil {
		return fmt.Errorf("write JSON: %w", err)
	}
	return nil
}

func writeResourcesTable(w io.Writer, rs []core.Resource, withReason bool) error {
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	header := "PROFILE\tREGION\tKIND\tNAME\tID\tSTATE\tPORTS"
	if withReason {
		header += "\tUNREACHABLE"
	}
	fmt.Fprintln(tw, header)
	for _, r := range rs {
		name := r.Name
		if r.IsBastion {
			name += " (bastion)"
		}
		line := strings.Join([]string{r.Profile, r.Region, string(r.Kind), name, r.ID, r.State, resourcePorts(r)}, "\t")
		if withReason {
			line += "\t" + r.Unreachable
		}
		fmt.Fprintln(tw, line)
	}
	if err := tw.Flush(); err != nil {
		return fmt.Errorf("write table: %w", err)
	}
	return nil
}

func resourcePorts(r core.Resource) string {
	if r.Kind == core.KindRDS {
		s := net.JoinHostPort(r.Host, strconv.Itoa(r.Port))
		if r.Engine != "" {
			s += " " + r.Engine
		}
		return s
	}
	parts := make([]string, 0, len(r.Ports))
	for _, p := range r.Ports {
		parts = append(parts, p.String())
	}
	return strings.Join(parts, ",")
}
