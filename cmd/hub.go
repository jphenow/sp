package cmd

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/jphenow/sp/internal/hub"
	"github.com/jphenow/sp/internal/setup"
	"github.com/jphenow/sp/internal/sprite"
)

const (
	// hubServiceName is the sprite-env service running the hub web app.
	hubServiceName = "sp-hub"
	// hubPort is the port the hub listens on inside its sprite.
	hubPort = 842
	// hubVaultPath is where the credential vault lives on the hub sprite.
	hubVaultPath = "/home/sprite/.config/sp/hub-vault.json"
)

var (
	hubAddr       string
	hubOrg        string
	hubToken      string
	hubHoldFor    time.Duration
	hubUseVault   bool
	hubAgentURL   string
	hubAgentName  string
	hubAgentEvery time.Duration
	hubDBPath     string
)

// hubCmd groups the control-surface commands: the web app that runs on one
// sprite, the reporting agent that runs on the others, and the local commands
// that put them there.
var hubCmd = &cobra.Command{
	Use:   "hub",
	Short: "A phone-friendly control surface for reviving sleeping sprites",
	Long: `hub runs a small web app on one sprite that lists every sprite in the org and
offers the revival ladder on each: wake the machine, start Claude, hold it
Active, hand it to Remote Control.

It exists because a laptop losing its connection puts sprites to sleep, and
there is otherwise no way to bring one back from a phone. The page is served at
the hub sprite's own URL, which is already behind the Sprites org login.

  sp hub deploy my-hub           # build, upload and start the hub on a sprite
  sp hub install gh-owner-repo   # install the reporting agent on a sprite
  sp hub serve                   # run the web app (runs ON the hub sprite)
  sp hub agent                   # run the reporter (runs ON a watched sprite)`,
}

// hubServeCmd runs the hub web app. Intended to run on the hub sprite as a
// sprite-env service, not on a laptop.
var hubServeCmd = &cobra.Command{
	Use:   "serve",
	Short: "Run the hub web app (runs ON the hub sprite)",
	RunE:  runHubServe,
}

// hubAgentCmd runs the reporting agent. Intended to run on a watched sprite as
// a sprite-env service.
var hubAgentCmd = &cobra.Command{
	Use:   "agent",
	Short: "Report this sprite's state to the hub (runs ON a watched sprite)",
	RunE:  runHubAgent,
}

// hubDeployCmd builds and installs the hub onto a sprite.
var hubDeployCmd = &cobra.Command{
	Use:   "deploy <sprite>",
	Short: "Build sp for linux, upload it to a sprite and start the hub service",
	Args:  cobra.ExactArgs(1),
	RunE:  runHubDeploy,
}

// hubInstallCmd installs the reporting agent onto a sprite.
var hubInstallCmd = &cobra.Command{
	Use:   "install <sprite>",
	Short: "Install the reporting agent on a sprite so the hub can show its state",
	Args:  cobra.ExactArgs(1),
	RunE:  runHubInstall,
}

func init() {
	hubServeCmd.Flags().StringVar(&hubAddr, "addr", fmt.Sprintf(":%d", hubPort), "listen address")
	hubServeCmd.Flags().StringVar(&hubOrg, "org", "", "Sprites organization (default: the CLI default)")
	hubServeCmd.Flags().DurationVar(&hubHoldFor, "hold-for", time.Hour, "how long a hold lasts when toggled on from the page")
	hubServeCmd.Flags().StringVar(&hubDBPath, "db", "", "hub database path (default: ~/.config/sp/hub.db)")
	hubServeCmd.Flags().BoolVar(&hubUseVault, "vault", false, "allow pushing stored Claude credentials to a sprite whose login expired")

	hubAgentCmd.Flags().StringVar(&hubAgentURL, "hub-url", "", "hub base URL (https://<hub>-<org>.sprites.app)")
	hubAgentCmd.Flags().StringVar(&hubAgentName, "sprite", "", "this sprite's name")
	hubAgentCmd.Flags().DurationVar(&hubAgentEvery, "interval", time.Minute, "how often to report")

	hubDeployCmd.Flags().StringVar(&hubOrg, "org", "", "Sprites organization (default: the CLI default)")
	hubDeployCmd.Flags().BoolVar(&hubUseVault, "vault", false, "also upload this machine's Claude credentials so the hub can repair expired logins")

	hubInstallCmd.Flags().StringVar(&hubOrg, "org", "", "Sprites organization (default: the CLI default)")
	hubInstallCmd.Flags().StringVar(&hubAgentURL, "hub-url", "", "hub base URL (default: looked up from the hub sprite)")
	hubInstallCmd.Flags().StringVar(&hubToken, "token", "", "Sprites org token for the agent (default: $SPRITE_API_TOKEN)")

	hubCmd.AddCommand(hubServeCmd, hubAgentCmd, hubDeployCmd, hubInstallCmd)
	rootCmd.AddCommand(hubCmd)
}

// runHubServe starts the web app, loading the credential vault when enabled.
func runHubServe(cmd *cobra.Command, args []string) error {
	store, err := hub.OpenStore(hubDBPath)
	if err != nil {
		return err
	}
	defer store.Close()

	var vault []byte
	if hubUseVault {
		vault, err = os.ReadFile(hubVaultPath)
		if err != nil {
			return fmt.Errorf("--vault is set but no credentials are stored at %s: %w", hubVaultPath, err)
		}
	}

	srv := hub.NewServer(hub.Config{
		Addr:    hubAddr,
		Org:     hubOrg,
		Token:   os.Getenv("SP_HUB_TOKEN"),
		HoldFor: hubHoldFor,
		Vault:   vault,
		Store:   store,
	})

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	return srv.Run(ctx)
}

// runHubAgent reports this sprite's ladder state to the hub on a loop.
func runHubAgent(cmd *cobra.Command, args []string) error {
	if hubAgentName == "" {
		hubAgentName = os.Getenv("SPRITE_NAME")
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	return hub.RunAgent(ctx, hub.AgentConfig{
		HubURL:   hubAgentURL,
		Token:    os.Getenv("SP_HUB_TOKEN"),
		Sprite:   hubAgentName,
		Interval: hubAgentEvery,
	})
}

// runHubDeploy builds a linux sp binary, uploads it to the hub sprite and
// registers the web app as a sprite-env service so it survives cold wakes.
func runHubDeploy(cmd *cobra.Command, args []string) error {
	spriteName := args[0]
	client := sprite.NewClient(hubOrg)

	exists, err := client.Exists(spriteName)
	if err != nil {
		return fmt.Errorf("checking sprite: %w", err)
	}
	if !exists {
		return fmt.Errorf("sprite %q does not exist — create it first", spriteName)
	}

	binary, cleanup, err := buildLinuxSp()
	if err != nil {
		return err
	}
	defer cleanup()

	fmt.Println("Uploading sp binary...")
	files := map[string]string{binary: "/usr/local/bin/sp"}
	serveArgs := fmt.Sprintf("hub,serve,--addr,:%d", hubPort)
	if hubOrg != "" {
		serveArgs += ",--org," + hubOrg
	}

	if hubUseVault {
		creds := setup.LocalClaudeCredentials()
		if len(creds) == 0 {
			return fmt.Errorf("--vault is set but no Claude credentials were found on this machine")
		}
		local, err := writeTempFile("sp-hub-vault-*.json", creds)
		if err != nil {
			return err
		}
		defer os.Remove(local)
		files[local] = hubVaultPath
		serveArgs += ",--vault"
		fmt.Println("Uploading Claude credentials to the hub vault...")
	}

	if _, err := client.Exec(sprite.ExecOptions{
		Sprite:  spriteName,
		Org:     hubOrg,
		Command: []string{"sh", "-c", "mkdir -p /home/sprite/.config/sp && chmod +x /usr/local/bin/sp"},
		Files:   files,
	}); err != nil {
		return fmt.Errorf("uploading to sprite: %w", err)
	}

	client.Exec(sprite.ExecOptions{
		Sprite:  spriteName,
		Org:     hubOrg,
		Command: []string{"sprite-env", "services", "delete", hubServiceName},
	})

	createCmd := fmt.Sprintf(
		"sprite-env services create %s --cmd /usr/local/bin/sp --args %s --http-port %d --duration 10s",
		hubServiceName, serveArgs, hubPort,
	)
	if out, err := client.Exec(sprite.ExecOptions{
		Sprite:  spriteName,
		Org:     hubOrg,
		Command: []string{"sh", "-c", createCmd},
	}); err != nil {
		return fmt.Errorf("creating hub service: %w\n%s", err, string(out))
	}

	fmt.Printf("\nHub running on sprite %q.\n", spriteName)
	if info, err := client.Get(spriteName); err == nil && info != nil && info.URL != "" {
		fmt.Printf("  %s\n", info.URL)
		fmt.Printf("\nInstall the reporting agent on sprites you want detail for:\n")
		fmt.Printf("  sp hub install <sprite> --hub-url %s\n", info.URL)
	}
	return nil
}

// runHubInstall uploads sp to a sprite and registers the reporting agent.
func runHubInstall(cmd *cobra.Command, args []string) error {
	spriteName := args[0]
	if hubAgentURL == "" {
		return fmt.Errorf("--hub-url is required (the hub sprite's URL, from 'sp hub deploy')")
	}
	if hubToken == "" {
		hubToken = os.Getenv("SPRITE_API_TOKEN")
	}
	if hubToken == "" {
		return fmt.Errorf("an org token is required: pass --token or set SPRITE_API_TOKEN\n" +
			"the agent posts to the hub's URL, which only accepts browser logins or an org token")
	}

	binary, cleanup, err := buildLinuxSp()
	if err != nil {
		return err
	}
	defer cleanup()

	fmt.Printf("Installing the reporting agent on %s...\n", spriteName)
	actions := hub.NewActions(hubOrg)
	if err := actions.InstallAgent(hub.InstallAgentOptions{
		Sprite:     spriteName,
		HubURL:     hubAgentURL,
		Token:      hubToken,
		BinaryPath: binary,
	}); err != nil {
		return err
	}
	fmt.Printf("Agent installed. %s will report to the hub while it is awake.\n", spriteName)
	return nil
}

// buildLinuxSp cross-compiles sp for the sprite and returns the binary path
// plus a cleanup function.
func buildLinuxSp() (string, func(), error) {
	srcDir, err := spSourceDir()
	if err != nil {
		return "", nil, err
	}
	out := filepath.Join(os.TempDir(), "sp-linux-amd64")
	fmt.Println("Building sp for the sprite (linux/amd64)...")
	build := exec.Command("go", "build", "-o", out, ".")
	build.Dir = srcDir
	build.Env = append(os.Environ(), "GOOS=linux", "GOARCH=amd64")
	if combined, err := build.CombinedOutput(); err != nil {
		return "", nil, fmt.Errorf("building sp binary: %w\n%s", err, string(combined))
	}
	return out, func() { os.Remove(out) }, nil
}

// writeTempFile writes data to a new temporary file and returns its path.
func writeTempFile(pattern string, data []byte) (string, error) {
	f, err := os.CreateTemp("", pattern)
	if err != nil {
		return "", fmt.Errorf("creating temp file: %w", err)
	}
	defer f.Close()
	if _, err := f.Write(data); err != nil {
		os.Remove(f.Name())
		return "", fmt.Errorf("writing temp file: %w", err)
	}
	return f.Name(), nil
}
