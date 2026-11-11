package login

import (
	"errors"
	"net"
	"net/http"
	"strconv"
	"time"
)

// shortLinkPort keeps the short link identical across logins when it is free.
const shortLinkPort = 17777

// startShortLink serves http://localhost:<port>/ as a redirect to target, on
// the loopback interfaces only, until stop is called. It prefers
// shortLinkPort and falls back to any free port.
func startShortLink(target string) (short string, stop func(), err error) {
	v4, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(shortLinkPort)))
	if err != nil {
		if v4, err = net.Listen("tcp", "127.0.0.1:0"); err != nil {
			return "", nil, err
		}
	}
	port := strconv.Itoa(v4.Addr().(*net.TCPAddr).Port)
	listeners := []net.Listener{v4}
	// Browsers may resolve localhost to ::1 first; serve it too when available.
	if v6, err := net.Listen("tcp", net.JoinHostPort("::1", port)); err == nil {
		listeners = append(listeners, v6)
	}
	srv := &http.Server{Handler: redirectHandler(target), ReadHeaderTimeout: 5 * time.Second}
	for _, ln := range listeners {
		go func() {
			if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
				_ = ln.Close()
			}
		}()
	}
	return "http://localhost:" + port, func() { _ = srv.Close() }, nil
}

// redirectHandler answers "/" with a temporary redirect to target.
func redirectHandler(target string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		http.Redirect(w, r, target, http.StatusFound)
	})
}
