package cli

import (
	"fmt"
	"io"

	"github.com/charmbracelet/lipgloss"

	"github.com/nhl4000/awsport/internal/awsx"
)

// splitPermissions separates the missing-IAM-permission failures inside err
// from the rest (nil when every failure was a missing permission).
func splitPermissions(err error) ([]*awsx.PermissionError, error) {
	return awsx.PermissionErrors(err), awsx.WithoutPermissionErrors(err)
}

// writePermissionWarnings prints one calm warning per denied call, e.g.
// "⚠ example-dev: Can't list RDS databases in ca-central-1 — missing
// rds:DescribeDBInstances", followed by one admin hint per permission set.
func writePermissionWarnings(w io.Writer, perms []*awsx.PermissionError) {
	if len(perms) == 0 {
		return
	}
	warn := lipgloss.NewRenderer(w).NewStyle().Foreground(lipgloss.Color("3")) // ANSI yellow, follows the terminal theme
	for _, p := range perms {
		fmt.Fprintln(w, warn.Render(permissionWarning(p)))
	}
	for _, h := range awsx.Hints(perms) {
		fmt.Fprintln(w, "  "+h)
	}
}

func permissionWarning(p *awsx.PermissionError) string {
	s := "⚠ "
	if p.Profile != "" {
		s += p.Profile + ": "
	}
	s += "Can't " + p.What
	if p.Region != "" {
		s += " in " + p.Region
	}
	return s + " — missing " + p.Action
}
