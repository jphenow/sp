package cmd

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/jphenow/sp/internal/relay"
)

var (
	relayBind string
	relayList string
)

// relayCmd groups the relay commands.
var relayCmd = &cobra.Command{
	Use:   "relay",
	Short: "Reach sprites' T3 services from a phone, waking them on connect",
}

// relayServeCmd runs the relay.
var relayServeCmd = &cobra.Command{
	Use:   "serve",
	Short: "Serve each listed sprite's T3 service on its own port",
	Long: `serve runs on a machine that stays awake, on the same tailnet as your phone,
and gives each sprite in the list a port of its own. Point the T3 app at
http://<this machine>:<port>; the first request through it wakes the sprite.

The list holds one "<port> <sprite>" per line:

  8899 gh-jphenow-sp
  8900 gh-jphenow-other

The credential comes from SPRITE_RELAY_TOKEN. Mint a narrow one rather than
handing over your login: privileges ["service.proxy"] limited to the sp-t3
service reaches T3 and nothing else -- no exec, no reading or destroying
sprites, no other services.

The organization needs the target_header_routing feature: the relay sends the
credential as X-Sprite-Authorization so that T3's own Authorization header
reaches T3 untouched, and without the feature the edge ignores it.

  sp relay serve --org my-org --bind "$(tailscale ip -4)"

--bind is required. The relay grants anything that can reach it the use of
its credential, so it should listen on the tailnet address and nowhere else.

This is sp bridge for the Sprites 2 runtime. bridge forwards through the
sprite CLI's port proxy, which Sprites 2 does not have, and wakes each
connection with an exec; the relay speaks HTTP to the edge directly.`,
	Args: cobra.NoArgs,
	RunE: runRelayServe,
}

func init() {
	relayServeCmd.Flags().StringVar(&relayBind, "bind", "", "address to listen on: this machine's tailnet IP (required)")
	relayServeCmd.Flags().StringVar(&relayList, "list", defaultRelayList(), "file of \"<port> <sprite>\" routes")
	relayCmd.AddCommand(relayServeCmd)
	rootCmd.AddCommand(relayCmd)
}

// defaultRelayList is where serve looks for routes when --list isn't given.
func defaultRelayList() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config", "sp", "relay.list")
}

// runRelayServe checks the configuration and serves until interrupted.
func runRelayServe(cmd *cobra.Command, args []string) error {
	if relayBind == "" {
		return fmt.Errorf("--bind is required: pass this machine's tailnet address, e.g. --bind \"$(tailscale ip -4)\"")
	}
	if globalOrg == "" {
		return fmt.Errorf("--org (or SP_ORG) is required: the relay builds each sprite's address from it")
	}
	token := strings.TrimSpace(os.Getenv("SPRITE_RELAY_TOKEN"))
	if token == "" {
		return fmt.Errorf("SPRITE_RELAY_TOKEN is not set")
	}

	f, err := os.Open(relayList)
	if err != nil {
		return fmt.Errorf("reading routes: %w", err)
	}
	routes, err := relay.ParseRoutes(f)
	f.Close()
	if err != nil {
		return fmt.Errorf("%s: %w", relayList, err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return relay.Serve(ctx, relayBind, globalOrg, token, routes)
}
