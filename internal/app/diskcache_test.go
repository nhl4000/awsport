package app

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/nhl4000/awsport/internal/core"
)

func TestDiskCache(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "discovery-cache.json")
	c := &diskCache{path: path}
	now := time.Now()
	regions := []string{"us-east-1", "us-west-2"}
	rs := []core.Resource{{Kind: core.KindEC2, Profile: "example-dev", Region: "us-east-1", ID: "i-0a", Name: "app",
		Ports: []core.PortSpec{{Name: "http", Remote: 80}}}}

	if _, ok := c.get("example-dev", regions, now); ok {
		t.Fatal("hit on a missing file")
	}
	c.put("example-dev", diskEntry{At: now.Add(-time.Minute), Regions: regions, Resources: rs})
	c.put("example-old", diskEntry{At: now.Add(-diskCacheMaxAge - 2*time.Minute), Regions: regions})

	e, ok := c.get("example-dev", regions, now)
	if !ok || len(e.Resources) != 1 || e.Resources[0].Ports[0].Remote != 80 || !e.At.Equal(now.Add(-time.Minute)) {
		t.Fatalf("get = %+v, %v", e, ok)
	}
	fi, err := os.Stat(path)
	if err != nil || (runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600) {
		t.Fatalf("cache file: %v, %v", fi, err)
	}
	if _, ok := c.get("example-dev", []string{"us-east-1"}, now); ok {
		t.Error("hit although the profile's regions changed")
	}
	if _, ok := c.get("example-dev", regions, now.Add(diskCacheMaxAge)); ok {
		t.Error("hit on an expired entry")
	}
	if _, ok := c.get("example-old", regions, now); ok {
		t.Error("expired entry survived a put")
	}

	c.clear()
	if _, ok := c.get("example-dev", regions, now); ok {
		t.Error("hit after clear")
	}

	if err := os.WriteFile(path, []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := c.get("example-dev", regions, now); ok {
		t.Error("hit on a corrupt file")
	}
	c.put("example-dev", diskEntry{At: now, Regions: regions, Resources: rs})
	if _, ok := c.get("example-dev", regions, now); !ok {
		t.Error("put did not replace a corrupt file")
	}
}

func TestDiskCacheDisabled(t *testing.T) {
	var nilCache *diskCache
	nilCache.put("example-dev", diskEntry{At: time.Now()})
	nilCache.clear()
	if _, ok := nilCache.get("example-dev", nil, time.Now()); ok {
		t.Fatal("nil cache hit")
	}
}
