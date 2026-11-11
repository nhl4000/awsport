// Package ports allocates stable, conflict-free local ports for tunnels.
package ports

import (
	"errors"
	"fmt"
	"net"
	"strconv"
)

const (
	defaultOffset = 10000
	maxPort       = 65535
	// minPort is the lowest port considered when the search wraps around;
	// privileged ports are never auto-assigned.
	minPort = 1024
)

// ErrNoFreePort is returned when no free, non-reserved port exists.
var ErrNoFreePort = errors.New("no free local port available")

// Free reports whether 127.0.0.1:port can be bound right now.
func Free(port int) bool {
	if port < 1 || port > maxPort {
		return false
	}
	l, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		return false
	}
	_ = l.Close()
	return true
}

// Default returns 10000+remote if <=65535, else remote.
func Default(remote int) int {
	if p := defaultOffset + remote; p <= maxPort {
		return p
	}
	return remote
}

// Assign returns the saved port for key if one exists, otherwise picks the
// first free port starting at Default(remote), skipping ports in reserved,
// saves it via save(key, port) and returns it. If the saved port is not Free
// (or is reserved), returns the next free non-reserved port after it with
// temporary=true and does NOT persist it.
func Assign(key string, remote int, saved map[string]int, reserved map[int]bool, save func(key string, port int) error) (port int, temporary bool, err error) {
	if p, ok := saved[key]; ok && p > 0 {
		if !reserved[p] && Free(p) {
			return p, false, nil
		}
		next, err := firstFree(p+1, reserved)
		if err != nil {
			return 0, false, fmt.Errorf("assign temporary port for %s (saved %d busy): %w", key, p, err)
		}
		return next, true, nil
	}
	p, err := firstFree(Default(remote), reserved)
	if err != nil {
		return 0, false, fmt.Errorf("assign port for %s: %w", key, err)
	}
	if err := save(key, p); err != nil {
		return 0, false, fmt.Errorf("save port %d for %s: %w", p, key, err)
	}
	return p, false, nil
}

// firstFree scans start..65535, then wraps to minPort..start-1.
func firstFree(start int, reserved map[int]bool) (int, error) {
	if start < minPort || start > maxPort {
		start = minPort
	}
	for p := start; p <= maxPort; p++ {
		if !reserved[p] && Free(p) {
			return p, nil
		}
	}
	for p := minPort; p < start; p++ {
		if !reserved[p] && Free(p) {
			return p, nil
		}
	}
	return 0, ErrNoFreePort
}
