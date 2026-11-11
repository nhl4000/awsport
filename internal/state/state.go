// Package state persists machine-written awsport state (saved ports,
// bastions, manual ports, TUI-saved groups) in state.json.
package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/nhl4000/awsport/internal/config"
	"github.com/nhl4000/awsport/internal/core"
)

// Data is the persisted state document.
type Data struct {
	Ports       map[string]int             `json:"ports"`        // keyed by PortKey
	Bastions    map[string]string          `json:"bastions"`     // "profile/region" -> instance ID
	ManualPorts map[string][]core.PortSpec `json:"manual_ports"` // "profile/region/<Name tag>" -> ports
	Groups      []core.Group               `json:"groups"`       // user-saved groups from TUI
}

// Store is a mutex-guarded handle on state.json.
type Store struct {
	mu   sync.Mutex
	path string
	data Data
}

// PortKey keys saved ports by profile, region, resource Name (not instance ID,
// so replaced instances keep their ports) and remote port.
func PortKey(profile, region, name string, remote int) string {
	return strings.Join([]string{profile, region, name, strconv.Itoa(remote)}, "/")
}

// Open loads config.StateDir()/state.json; a missing file yields empty state.
func Open() (*Store, error) {
	dir, err := config.StateDir()
	if err != nil {
		return nil, err
	}
	s := &Store{path: filepath.Join(dir, "state.json")}
	raw, err := os.ReadFile(s.path)
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return nil, fmt.Errorf("read state %s: %w", s.path, err)
	case len(strings.TrimSpace(string(raw))) > 0:
		if err := json.Unmarshal(raw, &s.data); err != nil {
			return nil, fmt.Errorf("parse state %s: %w", s.path, err)
		}
	}
	s.data.normalize()
	return s, nil
}

// Path returns the state file location.
func (s *Store) Path() string { return s.path }

// Get returns a deep copy of the current state.
func (s *Store) Get() Data {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.data.clone()
}

// Update applies fn to a copy of the state and atomically saves it. The
// in-memory state changes only if the save succeeds.
func (s *Store) Update(fn func(*Data)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := s.data.clone()
	fn(&next)
	next.normalize()
	raw, err := json.MarshalIndent(&next, "", "  ")
	if err != nil {
		return fmt.Errorf("encode state: %w", err)
	}
	if err := config.WriteFileAtomic(s.path, append(raw, '\n')); err != nil {
		return fmt.Errorf("save state: %w", err)
	}
	s.data = next
	return nil
}

// normalize ensures maps are non-nil so callers can assign into them.
func (d *Data) normalize() {
	if d.Ports == nil {
		d.Ports = map[string]int{}
	}
	if d.Bastions == nil {
		d.Bastions = map[string]string{}
	}
	if d.ManualPorts == nil {
		d.ManualPorts = map[string][]core.PortSpec{}
	}
}

func (d Data) clone() Data {
	out := Data{
		Ports:       make(map[string]int, len(d.Ports)),
		Bastions:    make(map[string]string, len(d.Bastions)),
		ManualPorts: make(map[string][]core.PortSpec, len(d.ManualPorts)),
	}
	for k, v := range d.Ports {
		out.Ports[k] = v
	}
	for k, v := range d.Bastions {
		out.Bastions[k] = v
	}
	for k, v := range d.ManualPorts {
		out.ManualPorts[k] = append([]core.PortSpec(nil), v...)
	}
	if d.Groups != nil {
		out.Groups = make([]core.Group, len(d.Groups))
		for i, g := range d.Groups {
			g.Members = append([]core.GroupMember(nil), g.Members...)
			out.Groups[i] = g
		}
	}
	return out
}
