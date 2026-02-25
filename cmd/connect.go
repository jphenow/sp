package cmd

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/jphenow/sp/internal/daemon"
	"github.com/jphenow/sp/internal/setup"
	"github.com/jphenow/sp/internal/sprite"
	"github.com/jphenow/sp/internal/store"
	spSync "github.com/jphenow/sp/internal/sync"
)

var (
	noSync      bool
	sessionName string
	webMode     bool
	webProxy    bool
	webDevPort  int
	execCmd     string
)

// connectCmd handles `sp .` and `sp owner/repo` — the core connect flow.
var connectCmd = &cobra.Command{
	Use:   "connect [target]",
	Short: "Connect to a sprite environment (default command for sp . or sp owner/repo)",
	Long: `Connect to a sprite environment with file syncing and a tmux session.

Target can be:
  .           Current directory (detects GitHub remote or uses dirname)
  owner/repo  A GitHub repository
  <name>      An existing sprite by name`,
	Args: cobra.MaximumNArgs(1),
	RunE: runConnect,
}

func init() {
	connectCmd.Flags().BoolVar(&noSync, "no-sync", false, "disable file syncing")
	connectCmd.Flags().StringVar(&sessionName, "name", "", "tmux session name")
	connectCmd.Flags().BoolVar(&webMode, "web", false, "enable opencode web UI via sprite service")
	connectCmd.Flags().BoolVar(&webProxy, "web-proxy", false, "enable reverse proxy in front of opencode (routes /opencode to opencode, /* to dev server)")
	connectCmd.Flags().IntVar(&webDevPort, "web-dev-port", 0, "development server port for proxy fallthrough (requires --web-proxy)")
	connectCmd.Flags().StringVar(&execCmd, "exec", "", "command to run instead of bash")

	// Register connect as both a subcommand and the default action
	rootCmd.AddCommand(connectCmd)
}

// resolveTarget determines what the user wants to connect to.
func resolveTarget(args []string) (*setup.ResolvedTarget, error) {
	target := "."
	if len(args) > 0 {
		target = args[0]
	}

	// Check if it's a path (. or starts with / or ./)
	if target == "." || strings.HasPrefix(target, "/") || strings.HasPrefix(target, "./") {
		return setup.ResolvePath(target)
	}

	// Check if it looks like owner/repo
	if strings.Contains(target, "/") {
		return setup.ResolveRepo(target)
	}

	// Treat as sprite name
	return &setup.ResolvedTarget{
		SpriteName: target,
		RemotePath: "/home/sprite",
	}, nil
}

// runConnect is the main connect flow, mirroring the Bash script's behavior.
func runConnect(cmd *cobra.Command, args []string) error {
	// Resolve target
	resolved, err := resolveTarget(args)
	if err != nil {
		return fmt.Errorf("resolving target: %w", err)
	}

	fmt.Printf("Connecting to sprite: %s\n", resolved.SpriteName)

	// Get Claude token
	tp := setup.NewTokenProvider()
	token, err := tp.GetToken()
	if err != nil {
		return fmt.Errorf("getting token: %w", err)
	}

	// Create sprite client
	client := sprite.NewClient(resolved.Org)

	// Check if sprite exists, create if needed
	exists, err := client.Exists(resolved.SpriteName)
	if err != nil {
		return fmt.Errorf("checking sprite: %w", err)
	}

	if !exists {
		fmt.Println("Creating sprite...")
		if err := client.Create(resolved.SpriteName); err != nil {
			return fmt.Errorf("creating sprite: %w", err)
		}

		// Wait for sprite to be ready
		fmt.Println("Waiting for sprite to be ready...")
		if err := waitForSpriteReady(client, resolved.SpriteName); err != nil {
			return fmt.Errorf("waiting for sprite: %w", err)
		}

		// Full setup for new sprites
		fmt.Println("Setting up authentication...")
		if err := setup.SetupSpriteAuth(client, resolved.SpriteName, token); err != nil {
			return fmt.Errorf("setting up auth: %w", err)
		}

		fmt.Println("Configuring git...")
		if err := setup.SetupGitConfig(client, resolved.SpriteName); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: git config failed: %v\n", err)
		}

		// Run setup.conf
		conf, err := setup.ParseSetupConf(setup.DefaultConfPath())
		if err != nil {
			fmt.Fprintf(os.Stderr, "Warning: parsing setup.conf: %v\n", err)
		}
		if conf != nil {
			fmt.Println("Running setup.conf...")
			setup.RunSetupConf(client, resolved.SpriteName, conf)
		}

		// Sync local directory for new sprites
		if resolved.LocalPath != "" && !noSync {
			fmt.Println("Uploading initial files...")
			if err := syncInitialFiles(client, resolved.SpriteName, resolved.LocalPath, resolved.RemotePath); err != nil {
				fmt.Fprintf(os.Stderr, "Warning: initial sync failed: %v\n", err)
			}
		}
	} else {
		// Existing sprite: just copy [always] files
		conf, err := setup.ParseSetupConf(setup.DefaultConfPath())
		if err == nil && conf != nil {
			alwaysFiles := setup.GetAlwaysFiles(conf)
			for _, f := range alwaysFiles {
				if _, err := os.Stat(f.Source); err == nil {
					client.Exec(sprite.ExecOptions{
						Sprite:  resolved.SpriteName,
						Command: []string{"true"},
						Files:   map[string]string{f.Source: f.Dest},
					})
				}
			}
		}
	}

	// Install the OSC 9999 open wrapper for tmux passthrough (idempotent)
	if err := setup.InstallOpenWrapper(client, resolved.SpriteName); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: open wrapper install failed: %v\n", err)
	}

	// Setup web service if requested — always reconfigure to ensure correct state
	if webMode {
		fmt.Println("Setting up web service...")
		if err := setupWebService(client, resolved.SpriteName); err != nil {
			return fmt.Errorf("setting up web service: %w", err)
		}
	}

	// Register with daemon. The daemon owns the sync lifecycle — it will
	// auto-start sync when the sprite is running and has local/remote paths.
	// In web mode the daemon is required; in console mode it's best-effort.
	if err := registerWithDaemon(resolved, client); err != nil {
		if webMode {
			return fmt.Errorf("daemon required for --web: %w", err)
		}
		fmt.Fprintf(os.Stderr, "Warning: daemon registration failed, sync will not persist: %v\n", err)

		// Fallback: inline sync for console mode when daemon is unavailable.
		// The proxy dies with this process, so it only works while sp is running.
		if !noSync && resolved.LocalPath != "" {
			fmt.Println("Starting inline file sync (no daemon)...")
			go func() {
				if err := startSyncInline(client, resolved); err != nil {
					fmt.Fprintf(os.Stderr, "Warning: sync setup failed: %v\n", err)
				}
			}()
		}
	} else if !noSync && resolved.LocalPath != "" {
		fmt.Println("Daemon will manage file sync.")
	}

	// In web mode, don't open a console — just return after setup.
	// The daemon manages sync from here.
	if webMode {
		info, _ := client.Get(resolved.SpriteName)
		if info != nil {
			fmt.Printf("\nSprite ready: %s\n", info.URL)
		}
		fmt.Println("The daemon will keep sync healthy while the sprite is running.")
		return nil
	}

	// Connect to sprite shell
	return execInSprite(client, resolved, token)
}

// defaultOpencodePort is the port opencode web listens on inside the sprite.
const defaultOpencodePort = 8080

// defaultProxyPort is the port the sp serve proxy listens on when --web-proxy is used.
const defaultProxyPort = 9000

// opencodeBin is the full path to the opencode binary on the sprite.
// sprite exec / sprite-env don't source .bashrc, so we can't rely on PATH.
const opencodeBin = "/home/sprite/.opencode/bin/opencode"

// setupWebService configures a sprite-env service for the opencode web UI with
// auto-wake on HTTP access. There are two modes:
//
// Direct mode (default): creates a service running `opencode web --port 8080`
// with --http-port 8080 so the sprite proxy routes directly to opencode.
//
// Proxy mode (--web-proxy): uploads the sp binary to the sprite and creates a
// service running `sp serve --opencode-port 8080 --proxy-port 9000` with
// --http-port 9000. This lets /opencode route to opencode and /* fall through
// to a dev server.
func setupWebService(client *sprite.Client, spriteName string) error {
	// Ensure opencode is installed. sprite exec doesn't source .bashrc, so
	// command -v opencode fails even when it's installed. Check the known
	// install path directly.
	_, err := client.Exec(sprite.ExecOptions{
		Sprite:  spriteName,
		Command: []string{"sh", "-c", fmt.Sprintf("test -x %s || (curl -fsSL https://opencode.ai/install | bash)", opencodeBin)},
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: opencode install check failed: %v\n", err)
	}

	// Delete any existing opencode/sp-web service to avoid conflicts
	client.Exec(sprite.ExecOptions{
		Sprite:  spriteName,
		Command: []string{"sprite-env", "services", "delete", "opencode"},
	})
	client.Exec(sprite.ExecOptions{
		Sprite:  spriteName,
		Command: []string{"sprite-env", "services", "delete", "sp-web"},
	})

	if webProxy {
		return setupWebServiceProxy(client, spriteName)
	}
	return setupWebServiceDirect(client, spriteName)
}

// setupWebServiceDirect creates a sprite service running opencode web directly
// on the HTTP port. The sprite proxy routes all traffic to opencode.
// Uses the full opencode binary path because sprite-env doesn't source .bashrc,
// and binds to 0.0.0.0 because the sprite proxy routes from outside localhost.
func setupWebServiceDirect(client *sprite.Client, spriteName string) error {
	port := defaultOpencodePort

	// Create the service with http-port for auto-wake.
	// --hostname 0.0.0.0 is required because the sprite proxy connects from
	// outside localhost; without it opencode binds to 127.0.0.1 only.
	createCmd := fmt.Sprintf(
		"sprite-env services create opencode --cmd %s --args web,--port,%d,--hostname,0.0.0.0 --http-port %d --duration 10s",
		opencodeBin, port, port,
	)

	out, err := client.Exec(sprite.ExecOptions{
		Sprite:  spriteName,
		Command: []string{"sh", "-c", createCmd},
	})
	if err != nil {
		return fmt.Errorf("creating opencode service: %w\n%s", err, string(out))
	}
	fmt.Printf("  opencode service created on port %d\n", port)

	// Get and display the sprite URL
	info, err := client.Get(spriteName)
	if err == nil && info != nil {
		fmt.Printf("  URL: %s\n", info.URL)
	}

	return nil
}

// setupWebServiceProxy uploads the sp binary to the sprite and creates a service
// running `sp serve` as a reverse proxy with /opencode routing and dev server fallthrough.
func setupWebServiceProxy(client *sprite.Client, spriteName string) error {
	oc := defaultOpencodePort
	pp := defaultProxyPort

	// Build the linux binary for the sprite
	fmt.Println("  Building sp binary for sprite (linux/amd64)...")
	buildCmd := exec.Command("go", "build", "-o", "/tmp/sp-linux-amd64", ".")
	buildCmd.Env = append(os.Environ(), "GOOS=linux", "GOARCH=amd64")
	if out, err := buildCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("building sp binary: %w\n%s", err, string(out))
	}
	defer os.Remove("/tmp/sp-linux-amd64")

	// Upload sp binary to the sprite
	fmt.Println("  Uploading sp binary to sprite...")
	if _, err := client.Exec(sprite.ExecOptions{
		Sprite:  spriteName,
		Command: []string{"sh", "-c", "chmod +x /usr/local/bin/sp"},
		Files:   map[string]string{"/tmp/sp-linux-amd64": "/usr/local/bin/sp"},
	}); err != nil {
		return fmt.Errorf("uploading sp binary: %w", err)
	}

	// Build the service args
	args := fmt.Sprintf("serve,--opencode-port,%d,--proxy-port,%d", oc, pp)
	if webDevPort > 0 {
		args += fmt.Sprintf(",--dev-port,%d", webDevPort)
	}

	createCmd := fmt.Sprintf(
		"sprite-env services create sp-web --cmd /usr/local/bin/sp --args %s --http-port %d --duration 10s",
		args, pp,
	)

	out, err := client.Exec(sprite.ExecOptions{
		Sprite:  spriteName,
		Command: []string{"sh", "-c", createCmd},
	})
	if err != nil {
		return fmt.Errorf("creating sp-web service: %w\n%s", err, string(out))
	}

	fmt.Printf("  sp-web proxy service created on port %d\n", pp)
	fmt.Printf("    /opencode -> localhost:%d (opencode web)\n", oc)
	if webDevPort > 0 {
		fmt.Printf("    /*        -> localhost:%d (dev server)\n", webDevPort)
	}

	// Get and display the sprite URL
	info, err := client.Get(spriteName)
	if err == nil && info != nil {
		fmt.Printf("  URL: %s\n", info.URL)
		fmt.Printf("  opencode: %s/opencode\n", info.URL)
	}

	return nil
}

// waitForSpriteReady polls until the sprite responds to commands.
func waitForSpriteReady(client *sprite.Client, name string) error {
	for i := 0; i < 60; i++ {
		_, err := client.Exec(sprite.ExecOptions{
			Sprite:  name,
			Command: []string{"echo", "ready"},
		})
		if err == nil {
			return nil
		}
		fmt.Print(".")
	}
	return fmt.Errorf("sprite did not become ready within 60 seconds")
}

// syncInitialFiles creates a tar of the local directory and uploads it to the sprite.
func syncInitialFiles(client *sprite.Client, name, localDir, remoteDir string) error {
	// Create temp tar file
	tmpFile, err := os.CreateTemp("", "sp-sync-*.tar.gz")
	if err != nil {
		return fmt.Errorf("creating temp file: %w", err)
	}
	defer os.Remove(tmpFile.Name())
	defer tmpFile.Close()

	// Use git ls-files if available to respect .gitignore
	var files []string
	gitCmd := exec.Command("git", "ls-files", "-co", "--exclude-standard")
	gitCmd.Dir = localDir
	out, err := gitCmd.Output()
	if err == nil {
		for _, f := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			if f != "" {
				files = append(files, f)
			}
		}
	} else {
		// Fallback: walk directory
		filepath.Walk(localDir, func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() {
				return nil
			}
			rel, _ := filepath.Rel(localDir, path)
			files = append(files, rel)
			return nil
		})
	}

	// Create tar.gz
	gw := gzip.NewWriter(tmpFile)
	tw := tar.NewWriter(gw)

	for _, f := range files {
		fullPath := filepath.Join(localDir, f)
		info, err := os.Stat(fullPath)
		if err != nil {
			continue
		}

		header, err := tar.FileInfoHeader(info, "")
		if err != nil {
			continue
		}
		header.Name = f

		if err := tw.WriteHeader(header); err != nil {
			continue
		}

		if !info.IsDir() {
			file, err := os.Open(fullPath)
			if err != nil {
				continue
			}
			io.Copy(tw, file)
			file.Close()
		}
	}

	tw.Close()
	gw.Close()
	tmpFile.Close()

	// Upload and extract
	client.Exec(sprite.ExecOptions{
		Sprite:  name,
		Command: []string{"mkdir", "-p", remoteDir},
	})

	_, err = client.Exec(sprite.ExecOptions{
		Sprite:  name,
		Command: []string{"sh", "-c", fmt.Sprintf("cd %s && tar xzf /tmp/upload.tar.gz", remoteDir)},
		Files:   map[string]string{tmpFile.Name(): "/tmp/upload.tar.gz"},
	})
	if err != nil {
		return fmt.Errorf("extracting files: %w", err)
	}

	// Clean up remote temp file
	client.Exec(sprite.ExecOptions{
		Sprite:  name,
		Command: []string{"rm", "-f", "/tmp/upload.tar.gz"},
	})

	return nil
}

// registerWithDaemon tells the daemon about this sprite for monitoring.
// Fetches the sprite's API info to populate ID, URL, and status.
func registerWithDaemon(resolved *setup.ResolvedTarget, client *sprite.Client) error {
	dc, err := daemon.Connect()
	if err != nil {
		return err
	}
	defer dc.Close()

	s := &store.Sprite{
		Name:       resolved.SpriteName,
		LocalPath:  resolved.LocalPath,
		RemotePath: resolved.RemotePath,
		Repo:       resolved.Repo,
		Org:        resolved.Org,
		Status:     "running",
		SyncStatus: "none",
	}

	// Fetch ID, URL, and real status from the API
	info, err := client.Get(resolved.SpriteName)
	if err == nil && info != nil {
		s.SpriteID = info.ID
		s.URL = info.URL
		if info.Status != "" {
			s.Status = info.Status
		}
	}

	return dc.UpsertSprite(s)
}

// startSyncInline runs sync setup directly in this process. The proxy is a
// child of sp, so it dies when sp exits. Only suitable for interactive console
// sessions where sp stays running.
func startSyncInline(client *sprite.Client, resolved *setup.ResolvedTarget) error {
	mgr := spSync.NewManager(client)

	// Setup SSH server on sprite
	if err := mgr.SetupSSHServer(resolved.SpriteName); err != nil {
		return fmt.Errorf("SSH server setup: %w", err)
	}

	// Start proxy
	_, port, err := mgr.StartProxy(resolved.SpriteName)
	if err != nil {
		return fmt.Errorf("starting proxy: %w", err)
	}

	// Add SSH config for Mutagen
	if err := spSync.AddSSHConfig(resolved.SpriteName, port); err != nil {
		return fmt.Errorf("adding SSH config: %w", err)
	}

	// Test SSH connection
	if err := spSync.TestSSHConnection(resolved.SpriteName, port, nil); err != nil {
		return fmt.Errorf("SSH connection test: %w", err)
	}

	// Start Mutagen sync
	mutagenID, err := mgr.StartMutagenSession(resolved.SpriteName, resolved.LocalPath, resolved.RemotePath)
	if err != nil {
		return fmt.Errorf("starting Mutagen: %w", err)
	}

	fmt.Printf("Sync started (mutagen: %s)\n", mutagenID)

	// Update daemon with sync info (best-effort)
	dc, dcErr := daemon.Connect()
	if dcErr == nil {
		dc.UpdateSyncStatus(resolved.SpriteName, "watching", "")
		dc.Close()
	}

	return nil
}

// execInSprite connects to the sprite with a tmux session.
func execInSprite(client *sprite.Client, resolved *setup.ResolvedTarget, token string) error {
	command := "bash"
	if execCmd != "" {
		command = execCmd
	}

	// Determine tmux session name
	tmuxSession := sessionName
	if tmuxSession == "" {
		tmuxSession = strings.ReplaceAll(command, " ", "-")
		tmuxSession = strings.ReplaceAll(tmuxSession, ".", "-")
		tmuxSession = strings.ReplaceAll(tmuxSession, ":", "-")
		// Use just the first word as session name
		if parts := strings.Fields(tmuxSession); len(parts) > 0 {
			tmuxSession = parts[0]
		}
	}

	shellCmd := fmt.Sprintf(
		"mkdir -p '%s' && cd '%s' && exec tmux new-session -A -s '%s' %s",
		resolved.RemotePath, resolved.RemotePath, tmuxSession, command,
	)

	env := map[string]string{
		"CLAUDE_CODE_OAUTH_TOKEN": token,
	}

	args := client.BuildExecArgs(sprite.ExecOptions{
		Sprite:  resolved.SpriteName,
		TTY:     true,
		Env:     env,
		Command: []string{"sh", "-c", shellCmd},
	})

	// Replace current process with sprite exec
	binary, err := exec.LookPath("sprite")
	if err != nil {
		return fmt.Errorf("sprite binary not found: %w", err)
	}

	// Use syscall.Exec-like behavior via os/exec
	cmd := exec.Command(binary, args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	runErr := cmd.Run()

	// Reset terminal after disconnect. Remote tmux may have enabled SGR mouse
	// tracking modes (1000/1003/1006) which persist after the connection drops,
	// causing raw escape sequences to appear on scroll/click.
	resetTerminal()

	return runErr
}

// resetTerminal disables mouse tracking modes that may have been left enabled
// by the remote tmux session. Without this, scrolling and clicking in the local
// terminal produces raw escape sequences like "65;40;62M".
func resetTerminal() {
	// Disable X10 mouse reporting (mode 1000)
	// Disable any-event mouse tracking (mode 1003)
	// Disable SGR extended mouse mode (mode 1006)
	fmt.Fprintf(os.Stderr, "\033[?1000l\033[?1003l\033[?1006l")
}
