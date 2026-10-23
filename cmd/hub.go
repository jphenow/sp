package cmd

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
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
	// hubPort is the port the hub listens on inside its sprite. It must be
	// above 1024: sprite-env services run as the unprivileged "sprite" user,
	// and a privileged port fails with "bind: permission denied". It also
	// avoids 8080 and 9000, which --web and --web-proxy already use.
	hubPort = 8420
	// hubVaultPath is where the credential vault lives on the hub sprite.
	hubVaultPath = "/home/sprite/.config/sp/hub-vault.json"
	// spRemotePath is where the sp binary is installed on a sprite.
	spRemotePath = "/usr/local/bin/sp"
	// spriteCLIDir is where the public installer puts the sprite CLI. It is
	// already on a sprite's PATH, and unlike /usr/local/bin it needs no sudo.
	spriteCLIDir = "/home/sprite/.local/bin"
	// spriteCLIInstaller is the public one-line installer for the sprite CLI,
	// which sprites do not ship with.
	spriteCLIInstaller = "https://sprites.dev/install.sh"
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
	hubDeployCmd.Flags().StringVar(&hubToken, "token", "", "sprite CLI token for the hub (default: $SPRITE_API_TOKEN)")
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
	if err := client.UploadBinary(spriteName, hubOrg, binary, spRemotePath); err != nil {
		return err
	}
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
		serveArgs += ",--vault"
		fmt.Println("Uploading Claude credentials to the hub vault...")
		if _, err := client.Exec(sprite.ExecOptions{
			Sprite:  spriteName,
			Org:     hubOrg,
			Command: []string{"sh", "-c", "mkdir -p /home/sprite/.config/sp"},
			Files:   map[string]string{local: hubVaultPath},
		}); err != nil {
			return fmt.Errorf("uploading the credential vault: %w", err)
		}
	}

	if err := installSpriteCLI(client, spriteName); err != nil {
		return err
	}

	client.Exec(sprite.ExecOptions{
		Sprite:  spriteName,
		Org:     hubOrg,
		Command: []string{"sprite-env", "services", "delete", hubServiceName},
	})

	// Services do not source a login profile, so the hub would not find the
	// sprite CLI or its config without being told where both live.
	env := fmt.Sprintf("HOME=/home/sprite,PATH=%s:/usr/local/bin:/usr/bin:/bin", spriteCLIDir)
	createCmd := fmt.Sprintf(
		"sprite-env services create %s --cmd /usr/local/bin/sp --args %s --env %s --http-port %d --duration 10s",
		hubServiceName, serveArgs, env, hubPort,
	)
	if out, err := client.Exec(sprite.ExecOptions{
		Sprite:  spriteName,
		Org:     hubOrg,
		Command: []string{"sh", "-c", createCmd},
	}); err != nil {
		return fmt.Errorf("creating hub service: %w\n%s", err, string(out))
	}

	fmt.Printf("\nHub running on sprite %q.\n", spriteName)
	url, err := hubServiceURL(client, spriteName)
	if err != nil {
		fmt.Printf("  (could not read the service URL: %v)\n", err)
		return nil
	}
	fmt.Printf("  %s\n", url)
	fmt.Printf("\nInstall the reporting agent on sprites you want detail for:\n")
	fmt.Printf("  sp hub install <sprite> --hub-url %s\n", url)
	return nil
}

// hubServiceURL reads the public URL of the hub service.
//
// Each service gets its own hostname — the sprite's own URL points at whatever
// service is primary, which for a sprite that already ran something else is not
// the hub. Reporting the sprite URL sent you to the wrong app.
func hubServiceURL(client *sprite.Client, spriteName string) (string, error) {
	out, err := client.Exec(sprite.ExecOptions{
		Sprite:  spriteName,
		Org:     hubOrg,
		Command: []string{"sprite-env", "services", "get", hubServiceName},
	})
	if err != nil {
		return "", fmt.Errorf("reading the hub service: %w", err)
	}
	var svc struct {
		PublicURL string `json:"public_url"`
		Service   struct {
			PublicURL string `json:"public_url"`
		} `json:"service"`
	}
	if err := json.Unmarshal(out, &svc); err != nil {
		return "", fmt.Errorf("parsing the service description: %w", err)
	}
	if svc.PublicURL != "" {
		return svc.PublicURL, nil
	}
	if svc.Service.PublicURL != "" {
		return svc.Service.PublicURL, nil
	}
	return "", fmt.Errorf("the service description carried no public URL")
}

// installSpriteCLI puts the sprite CLI on the hub and authenticates it.
//
// Sprites do not ship the CLI — /.sprite/bin has fly, flyctl and sprite-env,
// but not sprite — and sprite-env's own cross-sprite commands only reach
// sprites that this one created, so they cannot drive an existing fleet. The
// hub therefore needs the real CLI and a token of its own.
//
// Installing is skipped when the CLI is already present and authenticated, so
// a redeploy does not re-download it or need the token again.
func installSpriteCLI(client *sprite.Client, spriteName string) error {
	probe, _ := client.Exec(sprite.ExecOptions{
		Sprite:  spriteName,
		Org:     hubOrg,
		Command: []string{"sh", "-lc", spriteCLIDir + "/sprite list >/dev/null 2>&1 && echo READY || echo MISSING"},
	})
	if strings.Contains(string(probe), "READY") {
		fmt.Println("Sprite CLI already installed and authenticated.")
		return nil
	}

	if hubToken == "" {
		hubToken = os.Getenv("SPRITE_API_TOKEN")
	}
	if hubToken == "" {
		return fmt.Errorf("the hub needs its own sprite CLI token: pass --token or set SPRITE_API_TOKEN\n" +
			"generate one with 'sprite org auth' locally; it looks like org-slug/org-id/token-id/token-value")
	}

	fmt.Println("Installing and authenticating the sprite CLI on the hub...")
	script := fmt.Sprintf(`set -e
export SPRITE_INSTALL_BIN_DIR=%[1]s
mkdir -p "$SPRITE_INSTALL_BIN_DIR"
if [ ! -x "$SPRITE_INSTALL_BIN_DIR/sprite" ]; then
  curl -fsSL %[2]s | sh >/dev/null 2>&1
fi
"$SPRITE_INSTALL_BIN_DIR/sprite" auth setup --token %[3]s >/dev/null
"$SPRITE_INSTALL_BIN_DIR/sprite" list >/dev/null
`, spriteCLIDir, spriteCLIInstaller, shellQuoteArg(hubToken))

	if out, err := client.Exec(sprite.ExecOptions{
		Sprite:  spriteName,
		Org:     hubOrg,
		Command: []string{"sh", "-c", script},
	}); err != nil {
		return fmt.Errorf("installing the sprite CLI on %s: %w\n%s", spriteName, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// shellQuoteArg wraps a value in single quotes for safe use in a shell command.
func shellQuoteArg(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
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

// buildLinuxSp cross-compiles sp for the sprite, gzips it, and returns the
// path to the compressed binary plus a cleanup function.
//
// It is built with -s -w and shipped gzipped because sprite uploads go through
// a proxy that rejects large bodies with HTTP 413 — see Client.UploadBinary.
func buildLinuxSp() (string, func(), error) {
	srcDir, err := spSourceDir()
	if err != nil {
		return "", nil, err
	}
	out := filepath.Join(os.TempDir(), "sp-linux-amd64")
	fmt.Println("Building sp for the sprite (linux/amd64)...")
	build := exec.Command("go", "build", "-ldflags=-s -w", "-o", out, ".")
	build.Dir = srcDir
	build.Env = append(os.Environ(), "GOOS=linux", "GOARCH=amd64")
	if combined, err := build.CombinedOutput(); err != nil {
		return "", nil, fmt.Errorf("building sp binary: %w\n%s", err, string(combined))
	}
	defer os.Remove(out)

	gz := out + ".gz"
	if err := gzipFile(out, gz); err != nil {
		return "", nil, err
	}
	return gz, func() { os.Remove(gz) }, nil
}

// gzipFile compresses src into dst.
func gzipFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("reading built binary: %w", err)
	}
	defer in.Close()

	out, err := os.Create(dst)
	if err != nil {
		return fmt.Errorf("creating %s: %w", dst, err)
	}
	defer out.Close()

	zw, err := gzip.NewWriterLevel(out, gzip.BestCompression)
	if err != nil {
		return fmt.Errorf("starting gzip: %w", err)
	}
	if _, err := io.Copy(zw, in); err != nil {
		zw.Close()
		return fmt.Errorf("compressing binary: %w", err)
	}
	if err := zw.Close(); err != nil {
		return fmt.Errorf("finishing gzip: %w", err)
	}
	return nil
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
