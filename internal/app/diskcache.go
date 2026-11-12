package app

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/nhl4000/awsport/internal/awsx"
	"github.com/nhl4000/awsport/internal/config"
	"github.com/nhl4000/awsport/internal/core"
)

// diskCacheMaxAge bounds how old an on-disk discovery result may be to still
// be shown. Callers revalidate whatever they show from it.
const diskCacheMaxAge = 15 * time.Minute

// diskCache persists the last full discovery per profile so a new awsport
// process can show resources at once. It is disposable: read and write
// failures only mean a cache miss.
type diskCache struct {
	mu   sync.Mutex
	path string // empty disables the cache
}

type diskEntry struct {
	At        time.Time       `json:"at"`
	Regions   []string        `json:"regions"`
	Resources []core.Resource `json:"resources"`
}

func newDiskCache() *diskCache {
	dir, err := config.StateDir()
	if err != nil {
		return &diskCache{}
	}
	return &diskCache{path: filepath.Join(dir, "discovery-cache.json")}
}

func (c *diskCache) load() map[string]diskEntry {
	raw, err := os.ReadFile(c.path)
	if err != nil {
		return map[string]diskEntry{}
	}
	var entries map[string]diskEntry
	if json.Unmarshal(raw, &entries) != nil || entries == nil {
		return map[string]diskEntry{}
	}
	return entries
}

// get returns profile's entry when it was discovered for exactly regions and
// is at most diskCacheMaxAge old.
func (c *diskCache) get(profile string, regions []string, now time.Time) (diskEntry, bool) {
	if c == nil || c.path == "" {
		return diskEntry{}, false
	}
	c.mu.Lock()
	e, ok := c.load()[profile]
	c.mu.Unlock()
	if !ok || !slices.Equal(e.Regions, regions) || now.Sub(e.At) > diskCacheMaxAge || e.At.After(now) {
		return diskEntry{}, false
	}
	return e, true
}

// put stores profile's entry and drops expired ones.
func (c *diskCache) put(profile string, e diskEntry) {
	if c == nil || c.path == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	entries := c.load()
	for k, old := range entries {
		if e.At.Sub(old.At) > diskCacheMaxAge {
			delete(entries, k)
		}
	}
	entries[profile] = e
	raw, err := json.Marshal(entries)
	if err != nil {
		return
	}
	_ = config.WriteFileAtomic(c.path, raw)
}

// clear removes every entry.
func (c *diskCache) clear() {
	if c == nil || c.path == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	_ = os.Remove(c.path)
}

// CachedDiscovery returns the last full discovery of p saved by any awsport
// process, if it covered p's current regions and is recent, together with
// when it ran. The result may be stale; show it and revalidate with Discover.
func (a *App) CachedDiscovery(p core.Profile) ([]core.Resource, time.Time, bool) {
	e, ok := a.disk.get(p.Name, awsx.Regions(p, a.Cfg.Regions), time.Now())
	if !ok {
		return nil, time.Time{}, false
	}
	return e.Resources, e.At, true
}
