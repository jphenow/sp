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

	// Setup web service if requested — always reconfigure to ensure correct state
	if webMode {
		fmt.Println("Setting up web service...")
		if err := setupWebService(client, resolved.SpriteName); err != nil {
			return fmt.Errorf("setting up web service: %w", err)
		}
	}

	// Register with daemon
	if err := registerWithDaemon(resolved); err != nil {
		// Non-fatal: daemon might not be running
		if verbose {
			fmt.Fprintf(os.Stderr, "Warning: daemon registration failed: %v\n", err)
		}
	}

	// Start sync
	if !noSync && resolved.LocalPath != "" {
		if webMode {
			// In web mode, run sync synchronously so it's established before
			// we return. The daemon keeps it healthy after that.
			fmt.Println("Starting file sync...")
			if err := startSync(client, resolved); err != nil {
				fmt.Fprintf(os.Stderr, "Warning: sync setup failed: %v\n", err)
			}
		} else {
			// In console mode, start sync in the background so the shell
			// is available immediately.
			fmt.Println("Starting file sync...")
			go func() {
				if err := startSync(client, resolved); err != nil {
					fmt.Fprintf(os.Stderr, "Warning: sync setup failed: %v\n", err)
				}
			}()
		}
	}

	// In web mode, don't open a console — just return after setup.
	// The daemon monitors sync health from here.
	if webMode {
		info, _ := client.Get(resolved.SpriteName)
		if info != nil {
			fmt.Printf("\nSprite ready: %s\n", info.URL)
		}
		fmt.Println("Sync is running. The daemon will keep it healthy.")
		return nil
	}

	// Connect to sprite shell
	return execInSprite(client, resolved, token)
}

// defaultOpencodePort is the port opencode web listens on inside the sprite.
const defaultOpencodePort = 8080

// defaultProxyPort is the port the sp serve proxy listens on when --web-proxy is used.
const defaultProxyPort = 9000

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
	// Ensure opencode is installed
	_, err := client.Exec(sprite.ExecOptions{
		Sprite:  spriteName,
		Command: []string{"sh", "-c", "command -v opencode || (curl -fsSL https://opencode.ai/install | bash)"},
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
func setupWebServiceDirect(client *sprite.Client, spriteName string) error {
	port := defaultOpencodePort

	// Create the service with http-port for auto-wake
	createCmd := fmt.Sprintf(
		"sprite-env services create opencode --cmd opencode --args web,--port,%d --http-port %d --duration 10s",
		port, port,
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
func registerWithDaemon(resolved *setup.ResolvedTarget) error {
	dc, err := daemon.Connect()
	if err != nil {
		return err
	}
	defer dc.Close()

	return dc.UpsertSprite(&store.Sprite{
		Name:       resolved.SpriteName,
		LocalPath:  resolved.LocalPath,
		RemotePath: resolved.RemotePath,
		Repo:       resolved.Repo,
		Org:        resolved.Org,
		Status:     "running",
		SyncStatus: "none",
	})
}

// startSync sets up the SSH tunnel and Mutagen sync session.
func startSync(client *sprite.Client, resolved *setup.ResolvedTarget) error {
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
	if err := spSync.TestSSHConnection(resolved.SpriteName, port); err != nil {
		return fmt.Errorf("SSH connection test: %w", err)
	}

	// Start Mutagen sync
	mutagenID, err := mgr.StartMutagenSession(resolved.SpriteName, resolved.LocalPath, resolved.RemotePath)
	if err != nil {
		return fmt.Errorf("starting Mutagen: %w", err)
	}

	fmt.Printf("Sync started (mutagen: %s)\n", mutagenID)

	// Update daemon with sync info
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

	return cmd.Run()
}
