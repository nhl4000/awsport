package awsx

import (
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/nhl4000/awsport/internal/core"
)

var (
	sepSpace = regexp.MustCompile(`\s*([:@])\s*`)
	portSep  = regexp.MustCompile(`[,;\s]+`)
	// ssmDocName matches an SSM document name or ARN.
	ssmDocName = regexp.MustCompile(`^[A-Za-z0-9_.:/-]{3,256}$`)
)

// parsePortsTag parses an awsport:ports tag value: comma-separated
// "name:port" or bare "port" entries ("mailpit:8025, app:8080,9000"), each
// optionally bound to a custom SSM session document with "@document"
// ("web:8080@acme-web-port-forward", "9000@acme-port-forward").
// Whitespace around separators is ignored, and whitespace or ';' also
// separate entries (EC2 tag values with instance-metadata tags enabled cannot
// contain commas). Invalid entries are skipped.
func parsePortsTag(v string) []core.PortSpec {
	v = sepSpace.ReplaceAllString(strings.TrimSpace(v), "$1")
	var out []core.PortSpec
	for _, entry := range portSep.Split(v, -1) {
		if entry == "" {
			continue
		}
		doc := ""
		if i := strings.IndexByte(entry, '@'); i >= 0 {
			entry, doc = entry[:i], entry[i+1:]
			if !ssmDocName.MatchString(doc) {
				continue
			}
		}
		name, num := "", entry
		if i := strings.LastIndexByte(entry, ':'); i >= 0 {
			name, num = entry[:i], entry[i+1:]
		}
		port, err := strconv.Atoi(num)
		if err != nil || !validPort(port) {
			continue
		}
		out = append(out, core.PortSpec{Name: name, Remote: port, Document: doc})
	}
	return out
}

func validPort(p int) bool { return p > 0 && p <= 65535 }

// documentPorts returns the ports bound to a document by the config entries
// matching profile and the instance Name tag. When several entries match one
// port, the most specific wins: a profile-scoped entry beats an unscoped one,
// and an exact name beats a wildcard.
func documentPorts(docs []core.PortDocument, profile, name string) []core.PortSpec {
	var matched []core.PortDocument
	for _, d := range docs {
		if d.Document != "" && (d.Profile == "" || d.Profile == profile) && nameMatches(d.Name, name) {
			matched = append(matched, d)
		}
	}
	specificity := func(d core.PortDocument) int {
		s := 0
		if d.Profile != "" {
			s += 2
		}
		if !strings.Contains(d.Name, "*") {
			s++
		}
		return s
	}
	// mergePorts lets later entries win, so order least specific first.
	sort.SliceStable(matched, func(i, j int) bool { return specificity(matched[i]) < specificity(matched[j]) })
	out := make([]core.PortSpec, 0, len(matched))
	for _, d := range matched {
		out = append(out, core.PortSpec{Remote: d.Remote, Document: d.Document})
	}
	return out
}

// nameMatches reports whether name equals pattern, where one '*' in pattern
// matches any (possibly empty) run of characters. An empty name never
// matches.
func nameMatches(pattern, name string) bool {
	if name == "" {
		return false
	}
	prefix, suffix, wild := strings.Cut(pattern, "*")
	if !wild {
		return pattern == name
	}
	return len(name) >= len(prefix)+len(suffix) && strings.HasPrefix(name, prefix) && strings.HasSuffix(name, suffix)
}

// mergePorts merges port lists deduplicated by Remote. Later sources override
// an earlier entry; an empty Name or Document in the later entry keeps the
// earlier one, so a document from config survives a tag that only names the
// port and presets never erase a document. The result is sorted by Remote.
func mergePorts(sources ...[]core.PortSpec) []core.PortSpec {
	byRemote := map[int]core.PortSpec{}
	for _, src := range sources {
		for _, p := range src {
			if !validPort(p.Remote) {
				continue
			}
			if prev, ok := byRemote[p.Remote]; ok {
				if p.Name == "" {
					p.Name = prev.Name
				}
				if p.Document == "" {
					p.Document = prev.Document
				}
			}
			byRemote[p.Remote] = p
		}
	}
	out := make([]core.PortSpec, 0, len(byRemote))
	for _, p := range byRemote {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Remote < out[j].Remote })
	return out
}
