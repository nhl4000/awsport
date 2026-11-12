package agent

import (
	"fmt"
	"net/http"
	"path/filepath"

	"github.com/nhl4000/awsport/internal/config"
	"github.com/nhl4000/awsport/internal/tunnel"
)

// LogPath is StateDir()/agent.log.
func LogPath() (string, error) {
	dir, err := config.StateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, logName), nil
}

// Inspect asks the running agent for its Info and sessions. Unlike Client it
// never starts, replaces or streams from an agent, whatever its version. info
// is nil (and err nil) when no agent listens on the socket.
func Inspect() (*Info, []tunnel.Session, error) {
	sock, err := SocketPath()
	if err != nil {
		return nil, nil, err
	}
	if !dialable(sock) {
		return nil, nil, nil
	}
	c := newClient(sock, "")
	defer c.Close()
	var info Info
	if err := c.call(http.MethodGet, "/v1/info", nil, &info, requestTimeout); err != nil {
		return nil, nil, fmt.Errorf("query agent: %w", err)
	}
	var sessions []tunnel.Session
	if err := c.call(http.MethodGet, "/v1/sessions", nil, &sessions, requestTimeout); err != nil {
		return &info, nil, fmt.Errorf("query agent sessions: %w", err)
	}
	return &info, sessions, nil
}
