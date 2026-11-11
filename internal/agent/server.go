package agent

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/nhl4000/awsport/internal/core"
	"github.com/nhl4000/awsport/internal/tunnel"
)

// DefaultIdleTimeout is how long an agent without sessions keeps running.
const DefaultIdleTimeout = 30 * time.Second

const (
	// subscriberBuffer is the per-stream event backlog; a stream that falls
	// further behind is dropped and its client resyncs from a new snapshot.
	subscriberBuffer = 256
	maxRequestBody   = 1 << 20
	shutdownTimeout  = 3 * time.Second
)

var errShuttingDown = errors.New("agent is shutting down")

// Server exposes a tunnel.Manager over HTTP. It exits once it has had no
// sessions (active, failed or auth-expired) for IdleTimeout.
type Server struct {
	Manager     *tunnel.Manager
	Version     string
	IdleTimeout time.Duration // 0 means DefaultIdleTimeout
	Log         *log.Logger   // nil discards

	started  time.Time
	quit     chan struct{} // closed by POST /v1/shutdown
	quitOnce sync.Once
	done     chan struct{} // closed when event streams must end

	// life orders session launches against shutdown: launching handlers hold
	// it shared, the idle check and shutdown hold it exclusively.
	life     sync.RWMutex
	closing  bool
	lastBusy time.Time

	mu   sync.Mutex
	subs map[*subscriber]struct{}
}

type subscriber struct {
	ch   chan tunnel.Event
	dead chan struct{} // closed when the subscriber lagged and was dropped
}

// Serve handles requests on ln until the agent is idle, asked to shut down
// or ctx ends. It then stops every session, removes the socket and returns.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	idle := cmp.Or(s.IdleTimeout, DefaultIdleTimeout)
	if s.Log == nil {
		s.Log = log.New(io.Discard, "", 0)
	}
	s.started = time.Now()
	s.lastBusy = s.started
	s.quit = make(chan struct{})
	s.done = make(chan struct{})
	s.subs = map[*subscriber]struct{}{}

	sockPath := ln.Addr().String()
	var owned fs.FileInfo
	if fi, err := os.Lstat(sockPath); err == nil {
		owned = fi
	}

	pumpStop, pumpDone := make(chan struct{}), make(chan struct{})
	go s.pump(pumpStop, pumpDone)

	srv := &http.Server{Handler: s.handler(), ReadHeaderTimeout: 10 * time.Second, ErrorLog: s.Log}
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()
	s.Log.Printf("agent %s (pid %d) listening on %s", s.Version, os.Getpid(), sockPath)

	tick := time.NewTicker(min(max(idle/4, 5*time.Millisecond), time.Second))
	defer tick.Stop()
	var err error
	reason := ""
	for reason == "" {
		select {
		case <-ctx.Done():
			reason = "interrupted"
		case <-s.quit:
			reason = "shutdown requested"
		case err = <-serveErr:
			reason = "serve failed"
			err = fmt.Errorf("serve agent: %w", err)
		case now := <-tick.C:
			if s.idleExpired(now, idle) {
				reason = fmt.Sprintf("idle for %s", idle)
			}
		}
	}
	s.Log.Printf("shutting down: %s", reason)

	s.life.Lock()
	s.closing = true
	s.life.Unlock()
	if n := len(s.Manager.Sessions()); n > 0 {
		s.Log.Printf("stopping %d tunnel(s)", n)
	}
	_ = s.Manager.StopAll()
	// Deliver the Stopped events to the streams before ending them.
	close(pumpStop)
	<-pumpDone
	closeSocket(ln, sockPath, owned)
	close(s.done)
	sctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if serr := srv.Shutdown(sctx); serr != nil && !errors.Is(serr, net.ErrClosed) {
		s.Log.Printf("shutdown http server: %v", serr)
	}
	return err
}

// idleExpired reports whether the agent has had no sessions for idle and, if
// so, marks it closing so that no launch can slip in.
func (s *Server) idleExpired(now time.Time, idle time.Duration) bool {
	s.life.Lock()
	defer s.life.Unlock()
	for _, sess := range s.Manager.Sessions() {
		if sess.Status != tunnel.Stopped {
			s.lastBusy = now
			return false
		}
	}
	if now.Sub(s.lastBusy) < idle {
		return false
	}
	s.closing = true
	return true
}

// pump fans Manager events out to the subscribed streams. After stop it
// drains the events already queued and returns.
func (s *Server) pump(stop <-chan struct{}, done chan<- struct{}) {
	defer close(done)
	events := s.Manager.Events()
	last := map[string]tunnel.Status{}
	deliver := func(ev tunnel.Event) {
		if st := ev.Session.Status; last[ev.Key] != st {
			line := fmt.Sprintf("%s (%s): %s", ev.Session.Spec.Label, ev.Key, st)
			if ev.Session.Err != "" {
				line += ": " + ev.Session.Err
			}
			s.Log.Print(line)
			if st == tunnel.Stopped {
				delete(last, ev.Key)
			} else {
				last[ev.Key] = st
			}
		}
		s.broadcast(ev)
	}
	for {
		select {
		case ev := <-events:
			deliver(ev)
		case <-stop:
			for {
				select {
				case ev := <-events:
					deliver(ev)
				default:
					return
				}
			}
		}
	}
}

func (s *Server) broadcast(ev tunnel.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for sub := range s.subs {
		select {
		case sub.ch <- ev:
		default:
			delete(s.subs, sub)
			close(sub.dead)
		}
	}
}

func (s *Server) subscribe() *subscriber {
	sub := &subscriber{ch: make(chan tunnel.Event, subscriberBuffer), dead: make(chan struct{})}
	s.mu.Lock()
	s.subs[sub] = struct{}{}
	s.mu.Unlock()
	return sub
}

func (s *Server) unsubscribe(sub *subscriber) {
	s.mu.Lock()
	delete(s.subs, sub)
	s.mu.Unlock()
}

func (s *Server) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/info", s.handleInfo)
	mux.HandleFunc("GET /v1/sessions", s.handleSessions)
	mux.HandleFunc("GET /v1/events", s.handleEvents)
	mux.HandleFunc("POST /v1/start", s.handleStart)
	mux.HandleFunc("POST /v1/stop", s.handleStop)
	mux.HandleFunc("POST /v1/stopall", s.handleStopAll)
	mux.HandleFunc("POST /v1/restart", s.handleRestart)
	mux.HandleFunc("POST /v1/restart-auth", s.handleRestartAuth)
	mux.HandleFunc("POST /v1/shutdown", s.handleShutdown)
	return mux
}

func (s *Server) handleInfo(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, Info{
		Version:  s.Version,
		PID:      os.Getpid(),
		Started:  s.started,
		Sessions: len(s.Manager.Sessions()),
	})
}

func (s *Server) handleSessions(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, s.Manager.Sessions())
}

// handleEvents streams NDJSON events: a snapshot event per session, then
// every later change. Events the snapshot already reflects are skipped.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	if s.isClosing() {
		writeError(w, http.StatusServiceUnavailable, errShuttingDown)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, errors.New("streaming unsupported"))
		return
	}
	// Subscribe before the snapshot so that no change falls in between.
	sub := s.subscribe()
	defer s.unsubscribe(sub)
	sessions, rev := s.Manager.Snapshot()
	h := w.Header()
	h.Set("Content-Type", "application/x-ndjson")
	h.Set(headerSnapshot, strconv.Itoa(len(sessions)))
	h.Set(headerRev, strconv.FormatUint(rev, 10))
	w.WriteHeader(http.StatusOK)
	enc := json.NewEncoder(w)
	for _, sess := range sessions {
		if enc.Encode(tunnel.Event{Key: sess.Spec.Key, Session: sess}) != nil {
			return
		}
	}
	flusher.Flush()
	send := func(ev tunnel.Event) bool {
		if ev.Session.Rev <= rev {
			return true
		}
		return enc.Encode(ev) == nil
	}
	for {
		select {
		case ev := <-sub.ch:
			if !send(ev) {
				return
			}
			flusher.Flush()
		case <-sub.dead:
			return
		case <-r.Context().Done():
			return
		case <-s.done:
			for {
				select {
				case ev := <-sub.ch:
					if !send(ev) {
						return
					}
				default:
					flusher.Flush()
					return
				}
			}
		}
	}
}

func (s *Server) handleStart(w http.ResponseWriter, r *http.Request) {
	var spec core.TunnelSpec
	if !readJSON(w, r, &spec) {
		return
	}
	if spec.Key == "" {
		writeError(w, http.StatusBadRequest, errors.New("start tunnel: empty key"))
		return
	}
	err := s.launch(func() error { return s.Manager.Start(spec) })
	if err != nil {
		writeLaunchError(w, err, http.StatusConflict)
		return
	}
	s.Log.Printf("start %s (%s) on 127.0.0.1:%d", spec.Label, spec.Key, spec.LocalPort)
	writeJSON(w, mutationResponse{Rev: s.Manager.Rev()})
}

func (s *Server) handleStop(w http.ResponseWriter, r *http.Request) {
	var req keyRequest
	if !readJSON(w, r, &req) {
		return
	}
	_ = s.Manager.Stop(req.Key)
	writeJSON(w, mutationResponse{Rev: s.Manager.Rev()})
}

func (s *Server) handleStopAll(w http.ResponseWriter, _ *http.Request) {
	_ = s.Manager.StopAll()
	writeJSON(w, mutationResponse{Rev: s.Manager.Rev()})
}

func (s *Server) handleRestart(w http.ResponseWriter, r *http.Request) {
	var req keyRequest
	if !readJSON(w, r, &req) {
		return
	}
	err := s.launch(func() error { return s.Manager.Restart(req.Key) })
	if err != nil {
		code := http.StatusConflict
		if _, ok := s.Manager.Get(req.Key); !ok {
			code = http.StatusNotFound
		}
		writeLaunchError(w, err, code)
		return
	}
	writeJSON(w, mutationResponse{Rev: s.Manager.Rev()})
}

func (s *Server) handleRestartAuth(w http.ResponseWriter, _ *http.Request) {
	var keys []string
	err := s.launch(func() error {
		var err error
		keys, err = s.Manager.RestartAuthExpired()
		return err
	})
	if err != nil {
		writeLaunchError(w, err, http.StatusInternalServerError)
		return
	}
	writeJSON(w, mutationResponse{Rev: s.Manager.Rev(), Keys: keys})
}

func (s *Server) handleShutdown(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, struct{}{})
	s.quitOnce.Do(func() { close(s.quit) })
}

// launch runs fn, which may create sessions, unless the agent is closing.
func (s *Server) launch(fn func() error) error {
	s.life.RLock()
	defer s.life.RUnlock()
	if s.closing {
		return errShuttingDown
	}
	return fn()
}

func (s *Server) isClosing() bool {
	s.life.RLock()
	defer s.life.RUnlock()
	return s.closing
}

func writeLaunchError(w http.ResponseWriter, err error, code int) {
	if errors.Is(err, errShuttingDown) {
		code = http.StatusServiceUnavailable
	}
	writeError(w, code, err)
}

func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequestBody)).Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("decode request: %w", err))
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(errorResponse{Error: err.Error()})
}
