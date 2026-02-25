package serve

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"time"
)

// Config holds the configuration for the on-sprite reverse proxy.
type Config struct {
	ProxyPort    int    // Port for the reverse proxy to listen on (0 = disabled)
	OpencodePort int    // Port where opencode web UI runs
	DevPort      int    // Port for a development server to forward to (0 = no dev server)
	OpencodeCmd  string // Command to start opencode (default: "opencode web")
}

// Server manages the on-sprite reverse proxy and opencode process.
type Server struct {
	config     Config
	opencodePr *os.Process
}

// NewServer creates a new on-sprite server with the given configuration.
func NewServer(config Config) *Server {
	return &Server{config: config}
}

// Run starts the opencode web UI process and optionally the reverse proxy.
// It blocks until the context is cancelled.
func (s *Server) Run(ctx context.Context) error {
	// Start opencode web
	if err := s.startOpencode(ctx); err != nil {
		return fmt.Errorf("starting opencode: %w", err)
	}

	// If no proxy port, just wait for context cancellation
	if s.config.ProxyPort == 0 {
		<-ctx.Done()
		return nil
	}

	// Build and start the reverse proxy
	mux := http.NewServeMux()
	s.setupRoutes(mux)

	srv := &http.Server{
		Addr:    fmt.Sprintf(":%d", s.config.ProxyPort),
		Handler: mux,
	}

	// Graceful shutdown
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shutdownCtx)
	}()

	fmt.Printf("sp serve: proxy listening on :%d\n", s.config.ProxyPort)
	fmt.Printf("  /opencode -> localhost:%d (opencode web)\n", s.config.OpencodePort)
	if s.config.DevPort > 0 {
		fmt.Printf("  /*        -> localhost:%d (dev server)\n", s.config.DevPort)
	}

	if err := srv.ListenAndServe(); err != http.ErrServerClosed {
		return fmt.Errorf("proxy server: %w", err)
	}
	return nil
}

// setupRoutes configures the HTTP handler routing for the reverse proxy.
func (s *Server) setupRoutes(mux *http.ServeMux) {
	// Route /opencode to the opencode web UI
	opencodeURL, _ := url.Parse(fmt.Sprintf("http://localhost:%d", s.config.OpencodePort))
	opencodeProxy := httputil.NewSingleHostReverseProxy(opencodeURL)
	mux.HandleFunc("/opencode", s.handleOpencode(opencodeProxy))
	mux.HandleFunc("/opencode/", s.handleOpencode(opencodeProxy))

	// Route everything else to the dev server (if configured)
	if s.config.DevPort > 0 {
		devURL, _ := url.Parse(fmt.Sprintf("http://localhost:%d", s.config.DevPort))
		devProxy := httputil.NewSingleHostReverseProxy(devURL)
		mux.HandleFunc("/", s.handleDev(devProxy))
	} else {
		mux.HandleFunc("/", s.handleNoDev())
	}
}

// handleOpencode creates an HTTP handler that proxies requests to the opencode web UI.
// It strips the /opencode prefix before forwarding.
func (s *Server) handleOpencode(proxy *httputil.ReverseProxy) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Strip /opencode prefix for the upstream
		r.URL.Path = strings.TrimPrefix(r.URL.Path, "/opencode")
		if r.URL.Path == "" {
			r.URL.Path = "/"
		}
		proxy.ServeHTTP(w, r)
	}
}

// handleDev creates an HTTP handler that proxies requests to the development server.
func (s *Server) handleDev(proxy *httputil.ReverseProxy) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		proxy.ServeHTTP(w, r)
	}
}

// handleNoDev returns a handler that responds with 502 when no dev server is configured.
func (s *Server) handleNoDev() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		fmt.Fprintf(w, "No development server configured. Use --dev-port to specify one.\n")
	}
}

// startOpencode launches the opencode web process as a child.
func (s *Server) startOpencode(ctx context.Context) error {
	cmdStr := s.config.OpencodeCmd
	if cmdStr == "" {
		cmdStr = "opencode web"
	}

	parts := strings.Fields(cmdStr)
	if len(parts) == 0 {
		return fmt.Errorf("empty opencode command")
	}

	// Add port flag
	args := append(parts[1:], "--port", fmt.Sprintf("%d", s.config.OpencodePort))
	cmd := exec.CommandContext(ctx, parts[0], args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting opencode: %w", err)
	}

	s.opencodePr = cmd.Process

	// Monitor in background
	go func() {
		cmd.Wait()
	}()

	return nil
}

// Shutdown stops the opencode process and proxy.
func (s *Server) Shutdown() {
	if s.opencodePr != nil {
		s.opencodePr.Signal(os.Interrupt)
		s.opencodePr.Wait()
	}
}
