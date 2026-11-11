package tui

import "github.com/charmbracelet/lipgloss"

var (
	colorAccent = lipgloss.AdaptiveColor{Light: "#5A3FD6", Dark: "#9D8CFF"}
	colorOK     = lipgloss.AdaptiveColor{Light: "#1A7F37", Dark: "#3FB950"}
	colorWarn   = lipgloss.AdaptiveColor{Light: "#9A6700", Dark: "#D29922"}
	colorErr    = lipgloss.AdaptiveColor{Light: "#CF222E", Dark: "#F85149"}
	colorMuted  = lipgloss.AdaptiveColor{Light: "#6E7781", Dark: "#8B949E"}

	titleBarStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#FFFFFF")).Background(colorAccent).Padding(0, 1)
	subtleStyle   = lipgloss.NewStyle().Foreground(colorMuted)
	okStyle       = lipgloss.NewStyle().Foreground(colorOK)
	warnStyle     = lipgloss.NewStyle().Foreground(colorWarn)
	errStyle      = lipgloss.NewStyle().Foreground(colorErr)
	boldStyle     = lipgloss.NewStyle().Bold(true)
	cursorStyle   = lipgloss.NewStyle().Foreground(colorAccent).Bold(true)
	bannerStyle   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#FFFFFF")).Background(colorErr).Padding(0, 1)
	warnBanner    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#000000")).Background(colorWarn).Padding(0, 1)
	bodyStyle     = lipgloss.NewStyle().Padding(0, 2)
	codeStyle     = lipgloss.NewStyle().Foreground(colorAccent)
)
