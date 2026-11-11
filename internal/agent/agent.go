// Package agent runs tunnel sessions in a background process that outlives
// the TUI and CLI invocations that started them. The agent serves a small
// JSON API over HTTP on a unix-domain socket in the state directory; Client
// implements tunnel.Controller on top of it.
package agent

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"

	"github.com/nhl4000/awsport/internal/config"
	"github.com/nhl4000/awsport/internal/tunnel"
)

const (
	socketName = "agent.sock"
	logName    = "agent.log"
	// dialTimeout bounds the reachability check of the socket.
	dialTimeout = time.Second
)

// ErrRunning reports that another agent already serves the socket.
var ErrRunning = errors.New("agent already running")

// Info describes a running agent (GET /v1/info).
type Info struct {
	Version  string    `json:"version"`
	PID      int       `json:"pid"`
	Started  time.Time `json:"started"`
	Sessions int       `json:"sessions"`
}

type keyRequest struct {
	Key string `json:"key"`
}

// mutationResponse answers every state-changing request. Rev is the
// Manager's Rev after the change; Keys lists restarted sessions for
// /v1/restart-auth.
type mutationResponse struct {
	Rev  uint64   `json:"rev"`
	Keys []string `json:"keys,omitempty"`
}

type errorResponse struct {
	Error string `json:"error"`
}

// Headers of GET /v1/events: the body starts with headerSnapshot events (one
// per session) reflecting the Manager at Rev headerRev; every later event has
// a higher Rev.
const (
	headerSnapshot = "X-Awsport-Snapshot"
	headerRev      = "X-Awsport-Rev"
)

// SocketPath is StateDir()/agent.sock.
func SocketPath() (string, error) {
	dir, err := config.StateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, socketName), nil
}

// Running reports whether an agent accepts connections on the socket. It
// never starts one.
func Running() bool {
	sock, err := SocketPath()
	return err == nil && dialable(sock)
}

func dialable(sock string) bool {
	conn, err := net.DialTimeout("unix", sock, dialTimeout)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// Listen claims the socket at path. A socket nobody answers on is stale and
// replaced; when another agent answers, Listen returns ErrRunning.
func Listen(path string) (net.Listener, error) {
	if dialable(path) {
		return nil, ErrRunning
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("remove stale socket %s: %w", path, err)
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("listen on %s: %w", path, err)
	}
	// Serve removes the socket itself, and only while it is still ours.
	if ul, ok := ln.(*net.UnixListener); ok {
		ul.SetUnlinkOnClose(false)
	}
	// Windows ignores Unix permission bits; the 0700 directory protects it.
	if err := os.Chmod(path, 0o600); err != nil && runtime.GOOS != "windows" {
		_ = ln.Close()
		_ = os.Remove(path)
		return nil, fmt.Errorf("restrict socket %s: %w", path, err)
	}
	return ln, nil
}

// Run serves the agent on the socket in the state directory until it has
// been idle for DefaultIdleTimeout, is asked to shut down or ctx ends; then
// it stops every tunnel. It returns nil at once when another agent already
// serves the socket.
func Run(ctx context.Context, version string) error {
	sock, err := SocketPath()
	if err != nil {
		return err
	}
	dir := filepath.Dir(sock)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	ln, err := Listen(sock)
	if errors.Is(err, ErrRunning) {
		return nil
	}
	if err != nil {
		return err
	}
	logFile, err := os.OpenFile(filepath.Join(dir, logName), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		closeSocket(ln, sock, nil)
		return fmt.Errorf("open agent log: %w", err)
	}
	defer logFile.Close()
	logger := log.New(logFile, "", log.LstdFlags)

	cfg, err := config.Load()
	if err != nil {
		logger.Printf("load config: %v", err)
		closeSocket(ln, sock, nil)
		return fmt.Errorf("load config: %w", err)
	}
	awsPath := "aws"
	if p, err := exec.LookPath("aws"); err == nil {
		awsPath = p
	}
	srv := &Server{
		Manager: tunnel.NewManager(tunnel.Options{
			AWSPath:     awsPath,
			Reconnect:   cfg.ReconnectEnabled(),
			MaxFailures: cfg.MaxFail(),
		}),
		Version: version,
		Log:     logger,
	}
	if err := srv.Serve(ctx, ln); err != nil {
		logger.Printf("agent stopped: %v", err)
		return err
	}
	return nil
}

// closeSocket removes the socket file when it is still the one described by
// owned (any file at path when owned is nil) and closes ln. Removing first
// keeps clients from dialing a listener that is going away.
func closeSocket(ln net.Listener, path string, owned fs.FileInfo) {
	cur, err := os.Lstat(path)
	if err == nil && (owned == nil || os.SameFile(owned, cur)) {
		_ = os.Remove(path)
	}
	_ = ln.Close()
}
