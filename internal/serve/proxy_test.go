package serve

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestProxyRoutingOpencode(t *testing.T) {
	// Create a mock opencode backend
	opencodeSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("opencode:" + r.URL.Path))
	}))
	defer opencodeSrv.Close()

	// Create the proxy server with the mock's port
	srv := &Server{config: Config{
		OpencodePort: mustParsePort(opencodeSrv.URL),
		ProxyPort:    9999,
	}}

	mux := http.NewServeMux()
	srv.setupRoutes(mux)

	// Test /opencode route
	req := httptest.NewRequest("GET", "/opencode/test", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	resp := w.Result()
	body, _ := io.ReadAll(resp.Body)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	if string(body) != "opencode:/test" {
		t.Errorf("body = %q, want %q", string(body), "opencode:/test")
	}
}

func TestProxyRoutingNoDevServer(t *testing.T) {
	srv := &Server{config: Config{
		OpencodePort: 8080,
		ProxyPort:    9999,
		DevPort:      0, // no dev server
	}}

	mux := http.NewServeMux()
	srv.setupRoutes(mux)

	// Test / route without dev server
	req := httptest.NewRequest("GET", "/", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	resp := w.Result()
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", resp.StatusCode)
	}
}

func TestProxyRoutingWithDevServer(t *testing.T) {
	// Create a mock dev server
	devSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("dev:" + r.URL.Path))
	}))
	defer devSrv.Close()

	srv := &Server{config: Config{
		OpencodePort: 8080,
		ProxyPort:    9999,
		DevPort:      mustParsePort(devSrv.URL),
	}}

	mux := http.NewServeMux()
	srv.setupRoutes(mux)

	// Test / route with dev server
	req := httptest.NewRequest("GET", "/api/test", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	resp := w.Result()
	body, _ := io.ReadAll(resp.Body)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	if string(body) != "dev:/api/test" {
		t.Errorf("body = %q, want %q", string(body), "dev:/api/test")
	}
}

func TestOpencodePathStripping(t *testing.T) {
	// Create a mock opencode backend that echoes the path
	opencodeSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(r.URL.Path))
	}))
	defer opencodeSrv.Close()

	srv := &Server{config: Config{
		OpencodePort: mustParsePort(opencodeSrv.URL),
	}}

	mux := http.NewServeMux()
	srv.setupRoutes(mux)

	tests := []struct {
		path     string
		wantPath string
	}{
		{"/opencode", "/"},
		{"/opencode/", "/"},
		{"/opencode/ws", "/ws"},
		{"/opencode/api/v1", "/api/v1"},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			req := httptest.NewRequest("GET", tt.path, nil)
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, req)

			body, _ := io.ReadAll(w.Result().Body)
			if string(body) != tt.wantPath {
				t.Errorf("path = %q, want %q", string(body), tt.wantPath)
			}
		})
	}
}

// mustParsePort extracts the port from an httptest.Server URL.
func mustParsePort(rawURL string) int {
	// URL is like http://127.0.0.1:PORT
	for i := len(rawURL) - 1; i >= 0; i-- {
		if rawURL[i] == ':' {
			port := 0
			for _, c := range rawURL[i+1:] {
				port = port*10 + int(c-'0')
			}
			return port
		}
	}
	return 0
}
