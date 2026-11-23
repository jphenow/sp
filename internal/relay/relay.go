// Package relay puts sprites' web services behind plain host:port addresses on
// a machine that stays awake.
//
// A client that knows nothing about sprites -- T3 Code's phone app is the case
// this exists for -- cannot wake one. Its only door is the sprite's URL, which
// wants an org login the app cannot complete, and the tailnet cannot help,
// because a tailnet peer that isn't running doesn't answer. The relay is that
// door's doorman: it holds a Sprites credential, so a request through it
// passes the edge, and a request through the edge is what wakes a sprite.
// Connecting is the wake.
//
// The credential travels as X-Sprite-Authorization rather than Authorization,
// because the application has an Authorization of its own (T3 sends its
// session token there). Overwriting it hands T3 a request with no credential
// at all. The edge honours the separate header only for organizations with
// the target_header_routing feature on; without it the edge answers 302.
package relay

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	// Service is the sprite service the relay forwards to. It is fixed
	// because the relay's credential is minted for it and nothing else.
	Service = "sp-t3"

	// wakeTimeout bounds how long one request waits for a sprite to wake,
	// and wakeInterval how often it asks. A warm sprite answers within a
	// couple of seconds and a cold one within a few more.
	wakeTimeout  = 30 * time.Second
	wakeInterval = time.Second
)

// Route maps a port on the relay to a sprite.
type Route struct {
	Port   int
	Sprite string
}

// ParseRoutes reads one route per line, "<port> <sprite>", skipping blank lines
// and # comments. A port used twice is an error rather than last-one-wins,
// since either reading of it would silently send a phone to the wrong sprite.
func ParseRoutes(r io.Reader) ([]Route, error) {
	var routes []Route
	seen := map[int]bool{}
	sc := bufio.NewScanner(r)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 {
			return nil, fmt.Errorf("line %d: want \"<port> <sprite>\", got %q", n, line)
		}
		port, err := strconv.Atoi(fields[0])
		if err != nil || port < 1 || port > 65535 {
			return nil, fmt.Errorf("line %d: %q is not a port", n, fields[0])
		}
		if seen[port] {
			return nil, fmt.Errorf("line %d: port %d is used twice", n, port)
		}
		seen[port] = true
		routes = append(routes, Route{Port: port, Sprite: fields[1]})
	}
	return routes, sc.Err()
}

// Address returns the URL a client should use to reach sprite through the
// relay at host, or "" when no route serves that sprite.
func Address(host string, routes []Route, sprite string) string {
	for _, rt := range routes {
		if rt.Sprite == sprite {
			return "http://" + net.JoinHostPort(host, strconv.Itoa(rt.Port))
		}
	}
	return ""
}

// ServiceURL returns the edge address of the relayed service on a sprite.
func ServiceURL(sprite, org string) *url.URL {
	return &url.URL{
		Scheme: "https",
		Host:   Service + "--" + sprite + "--default--" + org + ".sprites.app",
	}
}

// Handler returns a reverse proxy to target that carries token as the Sprites
// credential and waits out a sprite's wake rather than passing its 503 on.
//
// transport is the round tripper used to reach the edge; nil means the
// default. Tests pass their own.
func Handler(target *url.URL, token string, transport http.RoundTripper) http.Handler {
	return handler(target, token, transport, wakeTimeout, wakeInterval)
}

// handler is Handler with the wake timings exposed, so tests can wait out a
// wake in milliseconds.
func handler(target *url.URL, token string, transport http.RoundTripper, timeout, interval time.Duration) http.Handler {
	if transport == nil {
		transport = http.DefaultTransport
	}
	return &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			// SetURL also points Host at the target: the edge routes on Host,
			// so it must be the service's own name, not the one the client
			// dialed.
			r.SetURL(target)
			// A client choosing its own target would let anyone who can reach
			// the relay spend its credential on any sprite the credential
			// covers. Only the relay's routes decide where a request goes.
			r.Out.Header.Del("X-Sprite-Target")
			r.Out.Header.Set("X-Sprite-Authorization", "Bearer "+token)
		},
		Transport: &wakeRetry{next: transport, timeout: timeout, interval: interval},
	}
}

// wakeRetry retries a request the edge answered with 503, which is what it
// says while a sprite is waking. Without it, the first request after a sprite
// sleeps fails, and a phone shows a failed attempt every time it reconnects.
type wakeRetry struct {
	next     http.RoundTripper
	timeout  time.Duration
	interval time.Duration
}

// RoundTrip sends req, resending it while the edge reports the sprite waking
// and the request is safe to send twice.
func (w *wakeRetry) RoundTrip(req *http.Request) (*http.Response, error) {
	start := time.Now()
	for tries := 1; ; tries++ {
		resp, err := w.next.RoundTrip(req)
		if err != nil || resp.StatusCode != http.StatusServiceUnavailable ||
			!replayable(req) || time.Since(start) >= w.timeout {
			if tries > 1 && err == nil && resp.StatusCode != http.StatusServiceUnavailable {
				log.Printf("%s woke after %s", req.URL.Host, time.Since(start).Round(100*time.Millisecond))
			}
			return resp, err
		}
		resp.Body.Close()
		select {
		case <-req.Context().Done():
			return nil, req.Context().Err()
		case <-time.After(w.interval):
		}
	}
}

// replayable reports whether req can be sent again unchanged: a read with no
// body. A request with a body cannot be resent once the first attempt has
// consumed it, and one that changes something may already have done so.
func replayable(req *http.Request) bool {
	if req.Method != http.MethodGet && req.Method != http.MethodHead {
		return false
	}
	return req.Body == nil || req.Body == http.NoBody
}

// Serve listens on bind at every route's port and relays each to its sprite
// until ctx ends or a listener fails.
//
// bind should be the relay's tailnet address. Listening everywhere would hand
// every sprite to every device on the local network with no credential at
// all, which is the opposite of the point.
func Serve(ctx context.Context, bind, org, token string, routes []Route) error {
	if len(routes) == 0 {
		return errors.New("no routes to serve")
	}
	errc := make(chan error, len(routes))
	var servers []*http.Server
	for _, rt := range routes {
		ln, err := net.Listen("tcp", net.JoinHostPort(bind, strconv.Itoa(rt.Port)))
		if err != nil {
			for _, s := range servers {
				s.Close()
			}
			return err
		}
		target := ServiceURL(rt.Sprite, org)
		srv := &http.Server{Handler: Handler(target, token, nil), ReadHeaderTimeout: 30 * time.Second}
		servers = append(servers, srv)
		log.Printf("%s -> %s", ln.Addr(), target.Host)
		go func() { errc <- srv.Serve(ln) }()
	}

	var err error
	select {
	case <-ctx.Done():
	case err = <-errc:
	}
	for _, s := range servers {
		s.Close()
	}
	return err
}
