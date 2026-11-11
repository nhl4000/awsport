// Package awsx wraps everything awsport needs from AWS: the shared config
// file (profiles, SSO sessions, awsport's managed block), SSO login and
// account listing, credential checks and EC2/RDS discovery.
package awsx

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/ini.v1"

	"github.com/nhl4000/awsport/internal/core"
)

// ConfigFilePath returns the AWS shared config path: $AWS_CONFIG_FILE or ~/.aws/config.
func ConfigFilePath() string {
	return envPathOr("AWS_CONFIG_FILE", "config")
}

// credentialsFilePath returns $AWS_SHARED_CREDENTIALS_FILE or ~/.aws/credentials.
func credentialsFilePath() string {
	return envPathOr("AWS_SHARED_CREDENTIALS_FILE", "credentials")
}

func envPathOr(env, name string) string {
	if p := os.Getenv(env); p != "" {
		return expandHome(p)
	}
	return filepath.Join(homeDir(), ".aws", name)
}

func homeDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return home
}

// DisplayPath shortens a path under the home directory to "~/..." for
// user-facing messages.
func DisplayPath(p string) string {
	home := homeDir()
	if home == "" {
		return p
	}
	if p == home {
		return "~"
	}
	if rel, ok := strings.CutPrefix(p, home+string(filepath.Separator)); ok {
		return filepath.Join("~", rel)
	}
	return p
}

func expandHome(p string) string {
	if p == "~" {
		return homeDir()
	}
	if strings.HasPrefix(p, "~/") || strings.HasPrefix(p, `~\`) {
		return filepath.Join(homeDir(), p[2:])
	}
	return p
}

// loadINI parses an AWS-style ini file. A missing file yields (nil, nil).
func loadINI(path string) (*ini.File, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	f, err := ini.LoadSources(ini.LoadOptions{
		AllowNestedValues:       true, // "s3 =\n  max_concurrent_requests = 10"
		IgnoreInlineComment:     true, // values (URLs) may contain '#' or ';'
		SkipUnrecognizableLines: true,
		AllowBooleanKeys:        true,
	}, data)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return f, nil
}

// configProfileName maps a config-file section name to a profile name.
func configProfileName(section string) (string, bool) {
	section = strings.TrimSpace(section)
	if section == "default" {
		return "default", true
	}
	if rest, ok := strings.CutPrefix(section, "profile "); ok {
		name := strings.TrimSpace(rest)
		return name, name != ""
	}
	return "", false
}

// ListProfiles returns every profile in the AWS config file ("default" and
// "[profile X]" sections) plus profiles that only exist in the credentials
// file, sorted by name.
func ListProfiles() ([]core.Profile, error) {
	path := ConfigFilePath()
	cfg, err := loadINI(path)
	if err != nil {
		return nil, err
	}
	creds, err := loadINI(credentialsFilePath())
	if err != nil {
		return nil, err
	}
	managed, err := managedSections(path)
	if err != nil {
		return nil, err
	}

	byName := map[string]*core.Profile{}
	var out []*core.Profile
	if cfg != nil {
		for _, sec := range cfg.Sections() {
			name, ok := configProfileName(sec.Name())
			if !ok {
				continue
			}
			p := byName[name]
			if p == nil {
				p = &core.Profile{Name: name}
				byName[name] = p
				out = append(out, p)
			}
			classify(p, sec, creds)
			p.Managed = managed[strings.TrimSpace(sec.Name())]
		}
	}
	if creds != nil {
		for _, sec := range creds.Sections() {
			name := strings.TrimSpace(sec.Name())
			if name == ini.DefaultSection || byName[name] != nil || !hasStaticKeys(sec) {
				continue
			}
			p := &core.Profile{Name: name, Kind: core.AuthStatic}
			byName[name] = p
			out = append(out, p)
		}
	}

	profiles := make([]core.Profile, 0, len(out))
	for _, p := range out {
		profiles = append(profiles, *p)
	}
	sort.Slice(profiles, func(i, j int) bool { return profiles[i].Name < profiles[j].Name })
	return profiles, nil
}

func classify(p *core.Profile, sec *ini.Section, creds *ini.File) {
	get := func(k string) string { return strings.TrimSpace(sec.Key(k).String()) }
	if v := get("region"); v != "" {
		p.Region = v
	}
	switch {
	case get("sso_session") != "" || get("sso_start_url") != "":
		p.Kind = core.AuthSSO
		p.SSOSession = get("sso_session")
		p.AccountID = get("sso_account_id")
		p.RoleName = get("sso_role_name")
	case get("role_arn") != "":
		p.Kind = core.AuthAssumeRole
		p.AccountID, p.RoleName = parseRoleARN(get("role_arn"))
	case hasStaticKeys(sec) || credsHaveKeys(creds, p.Name):
		p.Kind = core.AuthStatic
	default:
		p.Kind = core.AuthUnknown
	}
}

func hasStaticKeys(sec *ini.Section) bool {
	return strings.TrimSpace(sec.Key("aws_access_key_id").String()) != ""
}

func credsHaveKeys(creds *ini.File, name string) bool {
	if creds == nil {
		return false
	}
	sec, err := creds.GetSection(name)
	return err == nil && hasStaticKeys(sec)
}

// parseRoleARN extracts the account ID and role name from
// arn:<partition>:iam::<account>:role/<path/>name.
func parseRoleARN(arn string) (account, role string) {
	parts := strings.SplitN(arn, ":", 6)
	if len(parts) != 6 {
		return "", ""
	}
	account = parts[4]
	resource := parts[5]
	if i := strings.LastIndex(resource, "/"); i >= 0 {
		role = resource[i+1:]
	}
	return account, role
}

// FindSSOSession returns the first [sso-session X] section in the AWS config file.
func FindSSOSession() (name, startURL, region string, ok bool, err error) {
	cfg, err := loadINI(ConfigFilePath())
	if err != nil || cfg == nil {
		return "", "", "", false, err
	}
	for _, sec := range cfg.Sections() {
		rest, found := strings.CutPrefix(strings.TrimSpace(sec.Name()), "sso-session ")
		if !found || strings.TrimSpace(rest) == "" {
			continue
		}
		return strings.TrimSpace(rest),
			strings.TrimSpace(sec.Key("sso_start_url").String()),
			strings.TrimSpace(sec.Key("sso_region").String()),
			true, nil
	}
	return "", "", "", false, nil
}

// managedSections returns the trimmed section names declared inside awsport's
// managed block of the file at path.
func managedSections(path string) (map[string]bool, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return map[string]bool{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	inside, _ := scanSections(data)
	return inside, nil
}

// scanSections returns the trimmed section names declared inside awsport's
// managed block and those declared outside it.
func scanSections(data []byte) (inside, outside map[string]bool) {
	inside, outside = map[string]bool{}, map[string]bool{}
	in := false
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimRight(line, "\r")
		switch line {
		case BlockBegin:
			in = true
			continue
		case BlockEnd:
			in = false
			continue
		}
		name, ok := sectionHeader(line)
		if !ok {
			continue
		}
		if in {
			inside[name] = true
		} else {
			outside[name] = true
		}
	}
	return inside, outside
}

// sectionHeader parses "[name]" (surrounding whitespace and a trailing
// comment allowed) and returns the trimmed name.
func sectionHeader(line string) (string, bool) {
	t := strings.TrimSpace(line)
	if !strings.HasPrefix(t, "[") {
		return "", false
	}
	end := strings.LastIndexByte(t, ']')
	if end < 1 {
		return "", false
	}
	return strings.TrimSpace(t[1:end]), true
}
