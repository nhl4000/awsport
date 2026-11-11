package awsx

import (
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/nhl4000/awsport/internal/core"
)

var (
	colonSpace = regexp.MustCompile(`\s*:\s*`)
	portSep    = regexp.MustCompile(`[,;\s]+`)
)

// parsePortsTag parses an awsport:ports tag value: comma-separated
// "name:port" or bare "port" entries ("mailpit:8025, app:8080,9000").
// Whitespace around separators is ignored, and whitespace or ';' also
// separate entries (EC2 tag values with instance-metadata tags enabled cannot
// contain commas). Invalid entries are skipped.
func parsePortsTag(v string) []core.PortSpec {
	v = colonSpace.ReplaceAllString(strings.TrimSpace(v), ":")
	var out []core.PortSpec
	for _, entry := range portSep.Split(v, -1) {
		if entry == "" {
			continue
		}
		name, num := "", entry
		if i := strings.LastIndexByte(entry, ':'); i >= 0 {
			name, num = entry[:i], entry[i+1:]
		}
		port, err := strconv.Atoi(num)
		if err != nil || !validPort(port) {
			continue
		}
		out = append(out, core.PortSpec{Name: name, Remote: port})
	}
	return out
}

func validPort(p int) bool { return p > 0 && p <= 65535 }

// mergePorts merges port lists deduplicated by Remote. Later sources override
// the name of an earlier entry when they carry a non-empty name. The result
// is sorted by Remote.
func mergePorts(sources ...[]core.PortSpec) []core.PortSpec {
	byRemote := map[int]core.PortSpec{}
	for _, src := range sources {
		for _, p := range src {
			if !validPort(p.Remote) {
				continue
			}
			if prev, ok := byRemote[p.Remote]; ok && p.Name == "" {
				p.Name = prev.Name
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
