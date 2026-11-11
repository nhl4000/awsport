// Package deps detects awsport's external prerequisites (AWS CLI v2 and the
// Session Manager plugin) and knows the official commands to install them.
package deps

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Check is the detection result for one prerequisite.
type Check struct {
	Name    string
	Found   bool   // binary is on PATH
	Path    string // resolved binary path
	Version string // parsed version, e.g. "2.15.30"
	OK      bool   // found and usable
	Problem string // human explanation when !OK
	Install Install
}

// Install describes how to install a prerequisite on one platform. Commands
// is empty when there is no automated official install; DocsURL then has the
// manual instructions.
type Install struct {
	Description string
	Commands    [][]string // argv per step, run sequentially
	NeedsSudo   bool
	DocsURL     string
}

const (
	nameAWS    = "aws"
	namePlugin = "session-manager-plugin"

	awsDocsURL    = "https://docs.aws.amazon.com/cli/latest/userguide/getting-started-install.html"
	pluginDocsURL = "https://docs.aws.amazon.com/systems-manager/latest/userguide/session-manager-working-with-install-plugin.html"
	pluginBaseURL = "https://s3.amazonaws.com/session-manager-downloads/plugin/latest/"

	versionTimeout = 15 * time.Second
)

// osReleasePath and kernelReleasePath are variables so tests can point them
// at fixtures.
var (
	osReleasePath     = "/etc/os-release"
	kernelReleasePath = "/proc/sys/kernel/osrelease"
)

var (
	awsVersionRe    = regexp.MustCompile(`aws-cli/(\d+)\.(\d+)\.(\d+)`)
	pluginVersionRe = regexp.MustCompile(`\d+(?:\.\d+)+`)
)

// CheckAll detects the AWS CLI (version 2 required) and session-manager-plugin.
func CheckAll(ctx context.Context) []Check {
	wsl := IsWSL()
	return []Check{
		checkBinary(ctx, nameAWS, parseAWSVersion, InstallFor(nameAWS, runtime.GOOS, runtime.GOARCH, wsl)),
		checkBinary(ctx, namePlugin, parsePluginVersion, InstallFor(namePlugin, runtime.GOOS, runtime.GOARCH, wsl)),
	}
}

// checkBinary resolves name on PATH, runs `name --version` and validates the
// output with parse, which returns the version or why it is unusable.
func checkBinary(ctx context.Context, name string, parse func(string) (string, error), in Install) Check {
	c := Check{Name: name, Install: in}
	path, err := exec.LookPath(name)
	if err != nil {
		c.Problem = name + " not found in PATH"
		return c
	}
	c.Found, c.Path = true, path

	ctx, cancel := context.WithTimeout(ctx, versionTimeout)
	defer cancel()
	out, runErr := exec.CommandContext(ctx, path, "--version").CombinedOutput()
	v, err := parse(string(out))
	c.Version = v
	switch {
	case err == nil:
		c.OK = true
	case v == "" && runErr != nil:
		c.Problem = fmt.Sprintf("%s --version failed: %v", name, runErr)
	default:
		c.Problem = err.Error()
	}
	return c
}

// parseAWSVersion extracts the version from `aws --version` output
// ("aws-cli/2.15.30 Python/3.11.8 Linux/6.5.0 exe/x86_64.ubuntu.22"). It
// returns the version alongside an error when the major version is not 2.
func parseAWSVersion(out string) (string, error) {
	m := awsVersionRe.FindStringSubmatch(out)
	if m == nil {
		return "", fmt.Errorf("unrecognized aws --version output %q", strings.TrimSpace(out))
	}
	v := m[1] + "." + m[2] + "." + m[3]
	if major, _ := strconv.Atoi(m[1]); major != 2 {
		return v, fmt.Errorf("AWS CLI %s found; version 2 is required", v)
	}
	return v, nil
}

// parsePluginVersion extracts the version from `session-manager-plugin
// --version` output ("1.2.553.0").
func parsePluginVersion(out string) (string, error) {
	v := pluginVersionRe.FindString(out)
	if v == "" {
		return "", fmt.Errorf("unrecognized session-manager-plugin --version output %q", strings.TrimSpace(out))
	}
	return v, nil
}

// InstallFor returns the official install steps for name ("aws" or
// "session-manager-plugin") on goos/goarch. wsl marks Linux running under WSL,
// where the Linux build must be installed inside the distribution. On Linux
// the plugin package format follows the host's /etc/os-release.
func InstallFor(name, goos, goarch string, wsl bool) Install {
	var in Install
	switch name {
	case nameAWS:
		in = awsInstall(goos, goarch)
	case namePlugin:
		in = pluginInstall(goos, goarch)
	default:
		return Install{Description: fmt.Sprintf("unknown prerequisite %q", name)}
	}
	if wsl && goos == "linux" {
		in.Description += " (inside WSL: a Windows install is not visible to the Linux shell)"
	}
	return in
}

func awsInstall(goos, goarch string) Install {
	in := Install{DocsURL: awsDocsURL}
	switch goos {
	case "linux":
		arch, ok := map[string]string{"amd64": "x86_64", "arm64": "aarch64"}[goarch]
		if !ok {
			in.Description = fmt.Sprintf("No official AWS CLI v2 build for linux/%s; see the install guide.", goarch)
			return in
		}
		in.Description = fmt.Sprintf("Download the official AWS CLI v2 installer (linux %s) and install it with sudo.", arch)
		in.Commands = [][]string{
			{"curl", "-fsSL", "https://awscli.amazonaws.com/awscli-exe-linux-" + arch + ".zip", "-o", "/tmp/awscliv2.zip"},
			{"unzip", "-o", "-q", "/tmp/awscliv2.zip", "-d", "/tmp"},
			{"sudo", "/tmp/aws/install", "--update"},
		}
		in.NeedsSudo = true
	case "darwin":
		in.Description = "Download the official AWS CLI v2 macOS package and install it with sudo installer."
		in.Commands = [][]string{
			{"curl", "-fsSL", "https://awscli.amazonaws.com/AWSCLIV2.pkg", "-o", "/tmp/AWSCLIV2.pkg"},
			{"sudo", "installer", "-pkg", "/tmp/AWSCLIV2.pkg", "-target", "/"},
		}
		in.NeedsSudo = true
	case "windows":
		in.Description = "Run the official AWS CLI v2 MSI installer (Windows asks for elevation)."
		in.Commands = [][]string{{"msiexec.exe", "/i", "https://awscli.amazonaws.com/AWSCLIV2.msi"}}
	default:
		in.Description = fmt.Sprintf("No official AWS CLI v2 build for %s/%s; see the install guide.", goos, goarch)
	}
	return in
}

func pluginInstall(goos, goarch string) Install {
	in := Install{DocsURL: pluginDocsURL}
	switch goos {
	case "linux":
		return pluginLinuxInstall(goarch, linuxFamily())
	case "darwin":
		dir := map[string]string{"amd64": "mac", "arm64": "mac_arm64"}[goarch]
		if dir == "" {
			in.Description = fmt.Sprintf("No Session Manager plugin build for darwin/%s; see the install guide.", goarch)
			return in
		}
		pkg := "/tmp/session-manager-plugin.pkg"
		in.Description = "Download the signed Session Manager plugin package, install it and link it into /usr/local/bin."
		in.Commands = [][]string{
			{"curl", "-fsSL", pluginBaseURL + dir + "/session-manager-plugin.pkg", "-o", pkg},
			{"sudo", "installer", "-pkg", pkg, "-target", "/"},
			{"sudo", "mkdir", "-p", "/usr/local/bin"},
			{"sudo", "ln", "-sf", "/usr/local/sessionmanagerplugin/bin/session-manager-plugin", "/usr/local/bin/session-manager-plugin"},
		}
		in.NeedsSudo = true
	case "windows":
		in.Description = "Download and run the official Session Manager plugin installer (Windows asks for elevation)."
		in.Commands = [][]string{{
			"powershell.exe", "-NoProfile", "-ExecutionPolicy", "Bypass", "-Command",
			"$f = Join-Path $env:TEMP 'SessionManagerPluginSetup.exe'; " +
				"Invoke-WebRequest -UseBasicParsing -Uri '" + pluginBaseURL + "windows/SessionManagerPluginSetup.exe' -OutFile $f; " +
				"Start-Process -Wait -FilePath $f",
		}}
	default:
		in.Description = fmt.Sprintf("No Session Manager plugin build for %s/%s; see the install guide.", goos, goarch)
	}
	return in
}

func pluginLinuxInstall(goarch, family string) Install {
	in := Install{DocsURL: pluginDocsURL}
	if goarch != "amd64" && goarch != "arm64" {
		in.Description = fmt.Sprintf("No Session Manager plugin package for linux/%s; see the install guide.", goarch)
		return in
	}
	switch family {
	case "debian":
		dir := map[string]string{"amd64": "ubuntu_64bit", "arm64": "ubuntu_arm64"}[goarch]
		deb := "/tmp/session-manager-plugin.deb"
		in.Description = "Download the Session Manager plugin .deb package and install it with dpkg."
		in.Commands = [][]string{
			{"curl", "-fsSL", pluginBaseURL + dir + "/session-manager-plugin.deb", "-o", deb},
			{"sudo", "dpkg", "-i", deb},
		}
		in.NeedsSudo = true
	case "rhel":
		dir := map[string]string{"amd64": "linux_64bit", "arm64": "linux_arm64"}[goarch]
		in.Description = "Install the Session Manager plugin RPM package with yum."
		in.Commands = [][]string{
			{"sudo", "yum", "install", "-y", pluginBaseURL + dir + "/session-manager-plugin.rpm"},
		}
		in.NeedsSudo = true
	default:
		in.Description = "Unrecognized Linux distribution (no Debian/Ubuntu or RHEL-family ID in /etc/os-release); install the plugin manually."
	}
	return in
}

// linuxFamily classifies the host by /etc/os-release ID and ID_LIKE:
// "debian", "rhel" or "" when unknown.
func linuxFamily() string {
	f, err := os.Open(osReleasePath)
	if err != nil {
		return ""
	}
	defer f.Close()
	var ids []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		key, val, ok := strings.Cut(strings.TrimSpace(sc.Text()), "=")
		if !ok || (key != "ID" && key != "ID_LIKE") {
			continue
		}
		ids = append(ids, strings.Fields(strings.ToLower(strings.Trim(val, `"'`)))...)
	}
	debian := []string{"debian", "ubuntu"}
	rhel := []string{"rhel", "fedora", "centos", "amzn", "rocky", "almalinux", "ol"}
	for _, id := range ids {
		switch {
		case slices.Contains(debian, id):
			return "debian"
		case slices.Contains(rhel, id):
			return "rhel"
		}
	}
	return ""
}

// IsWSL reports whether the process runs on Linux under Windows Subsystem for Linux.
func IsWSL() bool {
	if runtime.GOOS != "linux" {
		return false
	}
	if os.Getenv("WSL_DISTRO_NAME") != "" {
		return true
	}
	b, err := os.ReadFile(kernelReleasePath)
	if err != nil {
		return false
	}
	r := strings.ToLower(string(b))
	return strings.Contains(r, "microsoft") || strings.Contains(r, "wsl")
}

// Run executes in.Commands sequentially with stdin attached, echoing each
// step to stdout. It stops at the first failing step.
func Run(ctx context.Context, in Install, stdout, stderr io.Writer) error {
	if len(in.Commands) == 0 {
		return errors.New("no automated install available; see " + in.DocsURL)
	}
	for i, argv := range in.Commands {
		if len(argv) == 0 {
			return fmt.Errorf("install step %d is empty", i+1)
		}
		fmt.Fprintf(stdout, "$ %s\n", strings.Join(argv, " "))
		cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
		cmd.Stdin = os.Stdin
		cmd.Stdout = stdout
		cmd.Stderr = stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("install step %d (%s): %w", i+1, argv[0], err)
		}
	}
	return nil
}
