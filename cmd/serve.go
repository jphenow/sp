package cmd

import (
	"context"
	"fmt"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/jphenow/sp/internal/serve"
)

var (
	opencodePort int
	devPort      int
	proxyPort    int
	opencodeCmd  string
)

// serveCmd runs the on-sprite reverse proxy. This is intended to run ON the sprite,
// not on your local machine.
var serveCmd = &cobra.Command{
	Use:   "serve",
	Short: "Run the on-sprite reverse proxy (runs ON the sprite)",
	Long: `Starts a reverse proxy on the sprite that routes traffic:

  /opencode/*  ->  opencode web UI (localhost:opencode-port)
  /*           ->  development server (localhost:dev-port, if configured)

Without --proxy-port, just starts opencode web on the opencode port directly
(for use as a sprite service without the proxy layer).

This command is intended to run ON the sprite, not on your local machine.
It is uploaded and started automatically when using 'sp . --web'.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		config := serve.Config{
			ProxyPort:    proxyPort,
			OpencodePort: opencodePort,
			DevPort:      devPort,
			OpencodeCmd:  opencodeCmd,
		}

		srv := serve.NewServer(config)

		ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		defer cancel()

		if proxyPort > 0 {
			fmt.Printf("Starting proxy on :%d\n", proxyPort)
		} else {
			fmt.Printf("Starting opencode web on :%d (no proxy)\n", opencodePort)
		}

		if err := srv.Run(ctx); err != nil {
			return fmt.Errorf("serve: %w", err)
		}

		srv.Shutdown()
		return nil
	},
}

func init() {
	serveCmd.Flags().IntVar(&opencodePort, "opencode-port", 8080, "port for opencode web UI")
	serveCmd.Flags().IntVar(&devPort, "dev-port", 0, "port for development server (0 = disabled)")
	serveCmd.Flags().IntVar(&proxyPort, "proxy-port", 0, "port for the reverse proxy (0 = direct opencode)")
	serveCmd.Flags().StringVar(&opencodeCmd, "opencode-cmd", "opencode web", "command to start opencode")
	rootCmd.AddCommand(serveCmd)
}
