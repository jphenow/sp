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

	// Register with daemon
	if err := registerWithDaemon(resolved); err != nil {
		// Non-fatal: daemon might not be running
		if verbose {
			fmt.Fprintf(os.Stderr, "Warning: daemon registration failed: %v\n", err)
		}
	}

	// Start sync in background
	if !noSync && resolved.LocalPath != "" {
		fmt.Println("Starting file sync...")
		go func() {
			if err := startSync(client, resolved); err != nil {
				fmt.Fprintf(os.Stderr, "Warning: sync setup failed: %v\n", err)
			}
		}()
	}

	// Connect to sprite shell
	return execInSprite(client, resolved, token)
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
