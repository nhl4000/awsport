package awsx

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/nhl4000/awsport/internal/config"
)

// Managed block markers (exact lines) delimiting the part of ~/.aws/config
// that awsport owns.
const (
	BlockBegin = "# >>> awsport managed (do not edit) >>>"
	BlockEnd   = "# <<< awsport managed <<<"
)

// DefaultSSOSession is the sso-session name used when config.SSO.Session is empty.
const DefaultSSOSession = "awsport"

// SSOProfile is one account/role pair reachable through IAM Identity Center.
type SSOProfile struct {
	Name, AccountID, AccountName, RoleName, Region string
}

// WriteManagedBlock replaces (or appends) the managed block in the AWS config
// file at path. The block holds one [sso-session <session>] and one
// [profile <Name>] per entry (sso_session, sso_account_id, sso_role_name,
// region, output=json), sorted by name. Bytes outside the block are never
// touched. Sections that already exist outside the block (a hand-written
// sso-session of the same name, or a same-named profile) are not duplicated
// inside it, since the AWS CLI rejects duplicate sections. The original file is
// copied to <path>.awsport.bak before each write; the write is atomic and the
// file mode is 0600. Writing identical content is a no-op.
func WriteManagedBlock(path string, sso config.SSO, profiles []SSOProfile) error {
	if strings.TrimSpace(sso.StartURL) == "" || strings.TrimSpace(sso.Region) == "" {
		return errors.New("write managed block: SSO start URL and region are required")
	}
	session := strings.TrimSpace(sso.Session)
	if session == "" {
		session = DefaultSSOSession
	}
	if err := checkValues(session, sso.StartURL, sso.Region); err != nil {
		return fmt.Errorf("write managed block: %w", err)
	}
	for _, p := range profiles {
		if err := checkValues(p.Name, p.AccountID, p.RoleName, p.Region); err != nil {
			return fmt.Errorf("write managed block: profile %q: %w", p.Name, err)
		}
		if strings.TrimSpace(p.Name) == "" {
			return errors.New("write managed block: profile with empty name")
		}
	}

	target := path
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		target = resolved // keep dotfile symlinks intact
	}
	orig, err := os.ReadFile(target)
	exists := err == nil
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("read %s: %w", path, err)
	}

	nl := "\n"
	if bytes.Contains(orig, []byte("\r\n")) {
		nl = "\r\n"
	}
	_, outside := scanSections(orig)
	block := renderBlock(session, sso, profiles, outside, nl)
	updated, err := spliceBlock(orig, block, nl)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	if exists && bytes.Equal(updated, orig) {
		return nil
	}
	if exists {
		if err := atomicWrite(path+".awsport.bak", orig); err != nil {
			return fmt.Errorf("backup %s: %w", path, err)
		}
	}
	if err := atomicWrite(target, updated); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

func checkValues(values ...string) error {
	for _, v := range values {
		if strings.ContainsAny(v, "\r\n[]") {
			return fmt.Errorf("invalid value %q", v)
		}
	}
	return nil
}

func renderBlock(session string, sso config.SSO, profiles []SSOProfile, outside map[string]bool, nl string) []byte {
	var sections [][]string
	if !outside["sso-session "+session] {
		sections = append(sections, []string{
			"[sso-session " + session + "]",
			"sso_start_url = " + strings.TrimSpace(sso.StartURL),
			"sso_region = " + strings.TrimSpace(sso.Region),
			"sso_registration_scopes = sso:account:access",
		})
	}

	sorted := append([]SSOProfile(nil), profiles...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })
	seen := map[string]bool{}
	for _, p := range sorted {
		name := strings.TrimSpace(p.Name)
		header := "profile " + name
		if name == "default" {
			header = "default"
		}
		if seen[name] || outside[header] {
			continue
		}
		seen[name] = true
		region := strings.TrimSpace(p.Region)
		if region == "" {
			region = strings.TrimSpace(sso.Region)
		}
		sections = append(sections, []string{
			"[" + header + "]",
			"sso_session = " + session,
			"sso_account_id = " + strings.TrimSpace(p.AccountID),
			"sso_role_name = " + strings.TrimSpace(p.RoleName),
			"region = " + region,
			"output = json",
		})
	}

	var b strings.Builder
	b.WriteString(BlockBegin + nl)
	for i, sec := range sections {
		if i > 0 {
			b.WriteString(nl)
		}
		for _, line := range sec {
			b.WriteString(line + nl)
		}
	}
	b.WriteString(BlockEnd + nl)
	return []byte(b.String())
}

// spliceBlock replaces the bytes from the begin-marker line through the
// end-marker line (inclusive of its line break) with block, or appends block
// after a blank line when no block exists.
func spliceBlock(orig, block []byte, nl string) ([]byte, error) {
	begin, end := -1, -1
	for off := 0; off < len(orig); {
		lineEnd := bytes.IndexByte(orig[off:], '\n')
		next := len(orig)
		if lineEnd >= 0 {
			next = off + lineEnd + 1
		}
		line := strings.TrimRight(string(orig[off:next]), "\r\n")
		switch {
		case begin < 0 && line == BlockBegin:
			begin = off
		case begin >= 0 && line == BlockEnd:
			end = next
		}
		if end >= 0 {
			break
		}
		off = next
	}

	if begin >= 0 {
		if end < 0 {
			return nil, errors.New("awsport managed block has a begin marker but no end marker; fix the file manually")
		}
		out := make([]byte, 0, len(orig)-(end-begin)+len(block))
		out = append(out, orig[:begin]...)
		out = append(out, block...)
		return append(out, orig[end:]...), nil
	}

	out := make([]byte, 0, len(orig)+2*len(nl)+len(block))
	out = append(out, orig...)
	if len(orig) > 0 {
		if orig[len(orig)-1] != '\n' {
			out = append(out, nl...)
		}
		out = append(out, nl...)
	}
	return append(out, block...), nil
}

// atomicWrite writes data to path via a temp file in the same directory and
// a rename, with mode 0600. Parent directories are created (0700).
func atomicWrite(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op after a successful rename
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// ProfileName builds "<account-name-slug>-<role-slug>", lowercase, [a-z0-9-] only.
func ProfileName(accountName, roleName string) string {
	var parts []string
	for _, s := range []string{slug(accountName), slug(roleName)} {
		if s != "" {
			parts = append(parts, s)
		}
	}
	return strings.Join(parts, "-")
}

func slug(s string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			if dash && b.Len() > 0 {
				b.WriteByte('-')
			}
			dash = false
			b.WriteRune(r)
			continue
		}
		dash = true
	}
	return b.String()
}
