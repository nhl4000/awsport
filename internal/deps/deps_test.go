package deps

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestParseAWSVersion(t *testing.T) {
	tests := []struct {
		out     string
		want    string
		wantErr bool
	}{
		{"aws-cli/2.15.30 Python/3.11.8 Linux/6.5.0-1016-aws exe/x86_64.ubuntu.22 prompt/off\n", "2.15.30", false},
		{"aws-cli/2.0.0 Python/3.7.3 Darwin/19.6.0 botocore/2.0.0dev4", "2.0.0", true},
		{"aws-cli/2.8.12 Python/3.9.11 Linux/5.15.0 exe/x86_64.ubuntu.22", "2.8.12", true},
		{"aws-cli/2.9.0 Python/3.9.11 Linux/5.15.0 exe/x86_64.ubuntu.22", "2.9.0", false},
		{"aws-cli/2.10.1 Python/3.9.11 Darwin/22.1.0 exe/x86_64", "2.10.1", false},
		{"aws-cli/1.29.62 Python/3.10.12 Linux/5.15.0 botocore/1.31.62", "1.29.62", true},
		{"command not found", "", true},
		{"", "", true},
	}
	for _, tt := range tests {
		got, err := parseAWSVersion(tt.out)
		if got != tt.want || (err != nil) != tt.wantErr {
			t.Errorf("parseAWSVersion(%q) = %q, %v; want %q, err=%v", tt.out, got, err, tt.want, tt.wantErr)
		}
	}
}

func TestParseAWSVersionTooOld(t *testing.T) {
	_, err := parseAWSVersion("aws-cli/2.7.35 Python/3.9.11 Linux/5.15.0 exe/x86_64.ubuntu.22")
	want := "AWS CLI 2.7.35 is too old for SSO sessions; awsport needs 2.9 or newer"
	if err == nil || err.Error() != want {
		t.Fatalf("err = %v; want %q", err, want)
	}
}

func TestCompareVersions(t *testing.T) {
	tests := []struct {
		a, b string
		want int
	}{
		{"2.9.0", "2.9.0", 0},
		{"2.9", "2.9.0", 0},
		{"2.8.12", "2.9.0", -1},
		{"2.10.0", "2.9.0", 1}, // numeric, not lexical
		{"2.9.1", "2.9.0", 1},
		{"1.99.99", "2.0.0", -1},
		{"3.0.0", "2.99.99", 1},
	}
	for _, tt := range tests {
		if got := compareVersions(tt.a, tt.b); got != tt.want {
			t.Errorf("compareVersions(%q, %q) = %d; want %d", tt.a, tt.b, got, tt.want)
		}
	}
}

func TestParsePluginVersion(t *testing.T) {
	tests := []struct {
		out     string
		want    string
		wantErr bool
	}{
		{"1.2.553.0\n", "1.2.553.0", false},
		{"session-manager-plugin version 1.2.707.0", "1.2.707.0", false},
		{"garbage", "", true},
	}
	for _, tt := range tests {
		got, err := parsePluginVersion(tt.out)
		if got != tt.want || (err != nil) != tt.wantErr {
			t.Errorf("parsePluginVersion(%q) = %q, %v; want %q, err=%v", tt.out, got, err, tt.want, tt.wantErr)
		}
	}
}

func writeOSRelease(t *testing.T, content string) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "os-release")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	old := osReleasePath
	osReleasePath = p
	t.Cleanup(func() { osReleasePath = old })
}

func TestLinuxFamily(t *testing.T) {
	tests := []struct {
		name, content, want string
	}{
		{"ubuntu", "NAME=\"Ubuntu\"\nID=ubuntu\nID_LIKE=debian\n", "debian"},
		{"debian", "ID=debian\n", "debian"},
		{"mint via ID_LIKE", "ID=linuxmint\nID_LIKE=\"ubuntu debian\"\n", "debian"},
		{"amazon linux", "ID=\"amzn\"\nID_LIKE=\"centos rhel fedora\"\n", "rhel"},
		{"rocky", "ID=\"rocky\"\nID_LIKE=\"rhel centos fedora\"\n", "rhel"},
		{"fedora", "ID=fedora\n", "rhel"},
		{"arch", "ID=arch\n", ""},
		{"empty", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			writeOSRelease(t, tt.content)
			if got := linuxFamily(); got != tt.want {
				t.Fatalf("linuxFamily() = %q, want %q", got, tt.want)
			}
		})
	}
	osReleasePath = filepath.Join(t.TempDir(), "missing")
	t.Cleanup(func() { osReleasePath = "/etc/os-release" })
	if got := linuxFamily(); got != "" {
		t.Fatalf("missing os-release: linuxFamily() = %q", got)
	}
}

func TestInstallFor(t *testing.T) {
	const deb = "ID=ubuntu\nID_LIKE=debian\n"
	const rpm = "ID=amzn\nID_LIKE=\"centos rhel fedora\"\n"
	tests := []struct {
		name, dep, goos, goarch, osRelease string
		wantURL                            string // substring of some command; "" = no commands
		wantSudo                           bool
		wantLast                           string // substring of the final step
	}{
		{"aws linux amd64", "aws", "linux", "amd64", deb, "https://awscli.amazonaws.com/awscli-exe-linux-x86_64.zip", true, "/tmp/aws/install --update"},
		{"aws linux arm64", "aws", "linux", "arm64", deb, "https://awscli.amazonaws.com/awscli-exe-linux-aarch64.zip", true, "/tmp/aws/install --update"},
		{"aws linux 386", "aws", "linux", "386", deb, "", false, ""},
		{"aws darwin arm64", "aws", "darwin", "arm64", "", "https://awscli.amazonaws.com/AWSCLIV2.pkg", true, "installer -pkg /tmp/AWSCLIV2.pkg -target /"},
		{"aws darwin amd64", "aws", "darwin", "amd64", "", "https://awscli.amazonaws.com/AWSCLIV2.pkg", true, "installer -pkg"},
		{"aws windows", "aws", "windows", "amd64", "", "https://awscli.amazonaws.com/AWSCLIV2.msi", false, "msiexec.exe /i"},
		{"aws freebsd", "aws", "freebsd", "amd64", "", "", false, ""},
		{"plugin ubuntu amd64", "session-manager-plugin", "linux", "amd64", deb, pluginBaseURL + "ubuntu_64bit/session-manager-plugin.deb", true, "dpkg -i"},
		{"plugin ubuntu arm64", "session-manager-plugin", "linux", "arm64", deb, pluginBaseURL + "ubuntu_arm64/session-manager-plugin.deb", true, "dpkg -i"},
		{"plugin rhel amd64", "session-manager-plugin", "linux", "amd64", rpm, pluginBaseURL + "linux_64bit/session-manager-plugin.rpm", true, "yum install -y"},
		{"plugin rhel arm64", "session-manager-plugin", "linux", "arm64", rpm, pluginBaseURL + "linux_arm64/session-manager-plugin.rpm", true, "yum install -y"},
		{"plugin unknown distro", "session-manager-plugin", "linux", "amd64", "ID=arch\n", "", false, ""},
		{"plugin linux 386", "session-manager-plugin", "linux", "386", deb, "", false, ""},
		{"plugin mac arm64", "session-manager-plugin", "darwin", "arm64", "", pluginBaseURL + "mac_arm64/session-manager-plugin.pkg", true, "ln -sf /usr/local/sessionmanagerplugin/bin/session-manager-plugin /usr/local/bin/session-manager-plugin"},
		{"plugin mac amd64", "session-manager-plugin", "darwin", "amd64", "", pluginBaseURL + "mac/session-manager-plugin.pkg", true, "ln -sf"},
		{"plugin windows", "session-manager-plugin", "windows", "amd64", "", pluginBaseURL + "windows/SessionManagerPluginSetup.exe", false, "Start-Process -Wait"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			writeOSRelease(t, tt.osRelease)
			in := InstallFor(tt.dep, tt.goos, tt.goarch, false)
			if in.DocsURL == "" || in.Description == "" {
				t.Fatalf("missing docs/description: %+v", in)
			}
			if tt.wantURL == "" {
				if len(in.Commands) != 0 {
					t.Fatalf("want no commands, got %q", in.Commands)
				}
				return
			}
			var all []string
			for _, c := range in.Commands {
				all = append(all, strings.Join(c, " "))
			}
			if !strings.Contains(strings.Join(all, "\n"), tt.wantURL) {
				t.Fatalf("commands %q lack %q", all, tt.wantURL)
			}
			if !strings.Contains(all[len(all)-1], tt.wantLast) {
				t.Fatalf("last step %q lacks %q", all[len(all)-1], tt.wantLast)
			}
			if in.NeedsSudo != tt.wantSudo {
				t.Fatalf("NeedsSudo = %v, want %v", in.NeedsSudo, tt.wantSudo)
			}
		})
	}
}

func TestInstallForWSLAndUnknown(t *testing.T) {
	if in := InstallFor("aws", "linux", "amd64", true); !strings.Contains(in.Description, "WSL") {
		t.Fatalf("WSL description = %q", in.Description)
	}
	if in := InstallFor("terraform", "linux", "amd64", false); len(in.Commands) != 0 || in.Description == "" {
		t.Fatalf("unknown dependency install = %+v", in)
	}
}

func TestIsWSL(t *testing.T) {
	if runtime.GOOS != "linux" {
		if IsWSL() {
			t.Fatal("IsWSL must be false off Linux")
		}
		return
	}
	tests := []struct {
		name, env, kernel string
		want              bool
	}{
		{"distro env", "Ubuntu", "6.8.0-generic", true},
		{"wsl2 kernel", "", "5.15.153.1-microsoft-standard-WSL2", true},
		{"wsl1 kernel", "", "4.4.0-19041-Microsoft", true},
		{"plain linux", "", "6.8.0-45-generic", false},
	}
	old := kernelReleasePath
	t.Cleanup(func() { kernelReleasePath = old })
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("WSL_DISTRO_NAME", tt.env)
			kernelReleasePath = filepath.Join(t.TempDir(), "osrelease")
			if err := os.WriteFile(kernelReleasePath, []byte(tt.kernel+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if got := IsWSL(); got != tt.want {
				t.Fatalf("IsWSL() = %v, want %v", got, tt.want)
			}
		})
	}
}
