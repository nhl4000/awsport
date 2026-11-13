package open

import (
	"sync"

	"github.com/nhl4000/awsport/internal/core"
)

// AutoOpener opens the browser for tunnels the user just started, once each
// is up. Only tunnels with a web URL (EC2 web ports, see URL) qualify, and
// each opens at most once: reconnects and tunnels that were already running
// never open a tab.
type AutoOpener struct {
	mu      sync.Mutex
	open    func(url string) error
	pending map[string]string // tunnel key -> URL
}

// NewAutoOpener returns an AutoOpener that opens URLs with open (Browser in
// production).
func NewAutoOpener(open func(url string) error) *AutoOpener {
	return &AutoOpener{open: open, pending: map[string]string{}}
}

// Expect registers a freshly started tunnel. It reports false when spec has
// no web URL (RDS, database or mail ports), so nothing will be opened.
func (a *AutoOpener) Expect(spec core.TunnelSpec) bool {
	u, ok := URL(spec)
	if !ok {
		return false
	}
	a.mu.Lock()
	a.pending[spec.Key] = u
	a.mu.Unlock()
	return true
}

// Update reports the current state of tunnel key. When an expected tunnel is
// up its URL is opened (once) and returned, with the error from opening it;
// when it ended without coming up it is forgotten. opened is false if no
// browser launch was attempted.
func (a *AutoOpener) Update(key string, up, active bool) (url string, opened bool, err error) {
	a.mu.Lock()
	u, ok := a.pending[key]
	if ok && (up || !active) {
		delete(a.pending, key)
	}
	a.mu.Unlock()
	if !ok || !up {
		return "", false, nil
	}
	return u, true, a.open(u)
}

// Pending reports how many expected tunnels have not come up yet.
func (a *AutoOpener) Pending() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.pending)
}
