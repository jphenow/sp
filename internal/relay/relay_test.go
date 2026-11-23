package relay

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestParseRoutes checks the list format and the mistakes it refuses rather
// than guesses at.
func TestParseRoutes(t *testing.T) {
	routes, err := ParseRoutes(strings.NewReader("# comment\n\n8899 gh-a\n  8900   gh-b  \n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(routes) != 2 || routes[0] != (Route{8899, "gh-a"}) || routes[1] != (Route{8900, "gh-b"}) {
		t.Fatalf("got %+v", routes)
	}

	for name, in := range map[string]string{
		"port used twice": "8899 gh-a\n8899 gh-b\n",
		"not a port":      "http gh-a\n",
		"port too large":  "70000 gh-a\n",
		"missing sprite":  "8899\n",
		"extra field":     "8899 gh-a gh-b\n",
	} {
		if _, err := ParseRoutes(strings.NewReader(in)); err == nil {
			t.Errorf("%s: accepted %q", name, in)
		}
	}
}

// TestAddress checks the lookup sp t3 pair uses to hand out a sprite's relay
// address.
func TestAddress(t *testing.T) {
	routes := []Route{{8899, "gh-a"}, {8900, "gh-b"}}
	if got := Address("daggett.example.ts.net", routes, "gh-b"); got != "http://daggett.example.ts.net:8900" {
		t.Errorf("got %q", got)
	}
	if got := Address("daggett.example.ts.net", routes, "gh-c"); got != "" {
		t.Errorf("unlisted sprite: got %q, want \"\"", got)
	}
}

// TestHandlerHeaders checks what reaches the edge: the relay's credential in
// its own header, the application's Authorization untouched, the edge's Host,
// and no target the client chose.
func TestHandlerHeaders(t *testing.T) {
	var got http.Header
	var gotHost string
	edge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, gotHost = r.Header.Clone(), r.Host
	}))
	defer edge.Close()
	target, _ := url.Parse(edge.URL)

	relay := httptest.NewServer(Handler(target, "relay-token", nil))
	defer relay.Close()

	req, _ := http.NewRequest("GET", relay.URL+"/api", nil)
	req.Header.Set("Authorization", "Bearer app-token")
	req.Header.Set("X-Sprite-Authorization", "Bearer forged")
	req.Header.Set("X-Sprite-Target", "sp-t3--someone-else--default--org")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	if v := got.Get("Authorization"); v != "Bearer app-token" {
		t.Errorf("application Authorization = %q, want it untouched", v)
	}
	if v := got.Get("X-Sprite-Authorization"); v != "Bearer relay-token" {
		t.Errorf("X-Sprite-Authorization = %q, want the relay's own", v)
	}
	if v := got.Get("X-Sprite-Target"); v != "" {
		t.Errorf("client-chosen X-Sprite-Target reached the edge: %q", v)
	}
	if gotHost != target.Host {
		t.Errorf("Host = %q, want the edge's %q", gotHost, target.Host)
	}
}

// TestWakeRetry checks that a waking sprite's 503s are waited out for a read,
// and passed straight back for a request that may not be sent twice.
func TestWakeRetry(t *testing.T) {
	var hits atomic.Int32
	edge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) <= 2 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		io.WriteString(w, "awake")
	}))
	defer edge.Close()
	target, _ := url.Parse(edge.URL)

	relay := httptest.NewServer(handler(target, "tok", nil, 5*time.Second, 10*time.Millisecond))
	defer relay.Close()

	t.Run("read waits for the wake", func(t *testing.T) {
		hits.Store(0)
		resp, err := http.Get(relay.URL)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 || string(body) != "awake" || hits.Load() != 3 {
			t.Fatalf("status %d body %q after %d edge requests; want 200 \"awake\" after 3",
				resp.StatusCode, body, hits.Load())
		}
	})
	t.Run("write is not resent", func(t *testing.T) {
		hits.Store(0)
		resp, err := http.Post(relay.URL, "text/plain", strings.NewReader("do it once"))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusServiceUnavailable || hits.Load() != 1 {
			t.Fatalf("status %d after %d edge requests; want 503 after exactly 1", resp.StatusCode, hits.Load())
		}
	})
}

// TestWebSocketPassesThrough checks that a protocol upgrade -- T3's session
// runs over a WebSocket -- survives the relay in both directions.
func TestWebSocketPassesThrough(t *testing.T) {
	edge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Upgrade") != "test-proto" {
			http.Error(w, "upgrade required", http.StatusUpgradeRequired)
			return
		}
		conn, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: test-proto\r\n\r\n")
		rw.Flush()
		line, _ := rw.ReadString('\n')
		rw.WriteString("echo " + line)
		rw.Flush()
	}))
	defer edge.Close()
	target, _ := url.Parse(edge.URL)
	relay := httptest.NewServer(Handler(target, "tok", nil))
	defer relay.Close()

	conn, err := net.Dial("tcp", strings.TrimPrefix(relay.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	io.WriteString(conn, "GET /ws HTTP/1.1\r\nHost: relay\r\nConnection: Upgrade\r\nUpgrade: test-proto\r\n\r\n")

	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("status %d, want 101", resp.StatusCode)
	}
	io.WriteString(conn, "ping\n")
	if line, _ := br.ReadString('\n'); line != "echo ping\n" {
		t.Fatalf("after upgrade got %q, want \"echo ping\\n\"", line)
	}
}
