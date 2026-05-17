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
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/jphenow/sp/internal/daemon"
	"github.com/jphenow/sp/internal/progress"
	"github.com/jphenow/sp/internal/setup"
	"github.com/jphenow/sp/internal/sprite"
	"github.com/jphenow/sp/internal/store"
	spSync "github.com/jphenow/sp/internal/sync"
)

var (
	noSync       bool
	sessionName  string
	webMode      bool
	webProxy     bool
	webDevPort   int
	execCmd      string
	keepWarmDur  time.Duration
)

// connectCmd handles `sp .` and `sp owner/repo` — the core connect flow.
var connectCmd = &cobra.Command{
	Use:   "connect [target] [variant]",
	Short: "Connect to a sprite environment (default command for sp . or sp owner/repo)",
	Long: `Connect to a sprite environment with file syncing and a tmux session.

Target can be:
  .           Current directory (detects GitHub remote or uses dirname)
  owner/repo  A GitHub repository
  <name>      An existing sprite by name

Optional variant is a free-form label that spawns a parallel sprite (the same
base repo/dir, under a distinct sprite name). Use variants to run short-run
ideas without disrupting your main sprite:
  sp .            scratch-idea      # fresh sprite for the current repo
  sp owner/repo   new-approach      # fresh sprite for owner/repo
For "sp . <variant>", the current dir is uploaded once at creation but no
ongoing sync runs — edits in the variant sprite stay in the sprite.`,
	Args: cobra.RangeArgs(0, 2),
	RunE: runConnect,
}

func init() {
	connectCmd.Flags().BoolVar(&noSync, "no-sync", false, "disable file syncing")
	connectCmd.Flags().StringVar(&sessionName, "name", "", "tmux session name")
	connectCmd.Flags().BoolVar(&webMode, "web", false, "enable opencode web UI via sprite service")
	connectCmd.Flags().BoolVar(&webProxy, "web-proxy", false, "enable reverse proxy in front of opencode (routes /opencode to opencode, /* to dev server)")
	connectCmd.Flags().IntVar(&webDevPort, "web-dev-port", 0, "development server port for proxy fallthrough (requires --web-proxy)")
	connectCmd.Flags().StringVar(&execCmd, "exec", "", "command to run instead of bash")
	connectCmd.Flags().DurationVar(&keepWarmDur, "keep-warm", 0, "spawn a background sentinel that holds the sprite warm for up to this duration after disconnect, exiting early if claude is idle for 60s (e.g. 1h, 30m). Default off.")

	// Register connect as both a subcommand and the default action
	rootCmd.AddCommand(connectCmd)
}

// resolveTarget determines what the user wants to connect to. The second
// positional argument, if present, is a free-form variant label that forks
// the sprite identity (see connectCmd.Long).
func resolveTarget(args []string) (*setup.ResolvedTarget, error) {
	target := "."
	variant := ""
	if len(args) > 0 {
		target = args[0]
	}
	if len(args) > 1 {
		variant = args[1]
	}

	// Check if it's a path (. or starts with / or ./)
	if target == "." || strings.HasPrefix(target, "/") || strings.HasPrefix(target, "./") {
		return setup.ResolvePath(target, variant)
	}

	// Check if it looks like owner/repo
	if strings.Contains(target, "/") {
		return setup.ResolveRepo(target, variant)
	}

	// Bare sprite name — variants only make sense with a base context, refuse.
	if variant != "" {
		return nil, fmt.Errorf("variant requires a path or owner/repo target, got bare sprite name %q", target)
	}
	return &setup.ResolvedTarget{
		SpriteName: target,
		BaseName:   target,
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

	// Sequential prologue: create + wait + perms must happen in order
	// before any parallel setup. Wrapped in a single spinner task so the
	// user sees elapsed time but we don't accidentally parallelize steps
	// that depend on each other (earlier bug: parallel perms-fix raced
	// with the ready-check and waited 55s instead of piggybacking).
	prologue := progress.New(verbose)
	prologue.Add("Preparing sprite", func() error {
		if !exists {
			if err := client.Create(resolved.SpriteName); err != nil {
				return fmt.Errorf("creating sprite: %w", err)
			}
		}
		if err := waitForSpriteReady(client, resolved.SpriteName); err != nil {
			return err
		}
		return setup.FixSpriteHomePermissions(client, resolved.SpriteName)
	})
	if err := prologue.Run(); err != nil {
		return err
	}

	// Claude auth path decision: prefer the env-token path (~/.claude-token
	// or CLAUDE_CODE_OAUTH_TOKEN) whenever a token is available. The
	// Keychain credentials.json path is the fallback for users who don't
	// have a setup-token but do have Claude Code creds in the local
	// Keychain. Reasons to prefer the token file:
	//
	//   - It's explicit user intent: if you ran `claude setup-token` and
	//     dropped the result in ~/.claude-token, that's the token you
	//     want propagated, not whatever Keychain happens to hold.
	//   - setup-tokens are long-lived and don't auto-refresh, so the
	//     "env goes stale across reconnects" failure mode doesn't apply
	//     in practice.
	//
	// Only fall back to pushing credentials.json when there's no token.
	var creds []byte
	authTokenForEnv := token
	if token == "" {
		creds = setup.LocalClaudeCredentials()
	}

	// Parallel setup. Five concurrent task chains:
	//
	//   1. Auth chain (SetupSpriteAuth → PushClaudeCredentials if creds →
	//      SetupGhAuth → InstallOpenWrapper). All four touch shell rc files,
	//      so they must serialize within this chain. Internal sed -i edits
	//      against .bashrc would race if these ran in parallel.
	//
	//   2. Git config (writes ~/.gitconfig only — no rc-file conflict).
	//
	//   3. Claude config chain (PushClaudeConfig → EnsureSpriteClaudeSettings).
	//      Both touch ~/.claude/, but only settings.json is shared between
	//      them and Ensure must layer on top of the pushed file.
	//
	//   4. Repo clone (writes /home/sprite/<repo>/ only — independent).
	//
	//   5. New-sprite-only setup.conf + initial file upload, OR for existing
	//      sprites the lighter "[always] files" copy. Independent.
	parallel := progress.New(verbose)

	parallel.Add("Setting up auth + gh + clone", func() error {
		// Auth must complete before clone because the clone needs the SSH
		// key and StrictHostKeyChecking=no config that auth deploys. These
		// are chained in one parallel task so other tasks (config push,
		// git config) run concurrently while this chain serializes.
		if err := setup.SetupSpriteAuth(client, resolved.SpriteName); err != nil {
			return fmt.Errorf("sprite auth: %w", err)
		}
		if creds != nil {
			if err := setup.PushClaudeCredentials(client, resolved.SpriteName, creds); err != nil {
				return fmt.Errorf("claude credentials: %w", err)
			}
		}
		if err := setup.SetupGhAuth(client, resolved.SpriteName); err != nil {
			return fmt.Errorf("gh auth: %w", err)
		}
		if err := setup.InstallOpenWrapper(client, resolved.SpriteName); err != nil {
			return fmt.Errorf("open wrapper: %w", err)
		}
		// Clone depends on SSH key from auth. Runs here instead of as a
		// separate parallel task to avoid the race where clone fires
		// before the key is deployed.
		if resolved.Repo != "" && resolved.LocalPath == "" {
			if err := cloneRepoOnSprite(client, resolved.SpriteName, resolved.Repo, resolved.RemotePath); err != nil {
				return fmt.Errorf("clone repo: %w", err)
			}
		}
		return nil
	})

	parallel.Add("Configuring git", func() error {
		return setup.SetupGitConfig(client, resolved.SpriteName)
	})

	parallel.Add("Syncing Claude config", func() error {
		if err := setup.PushClaudeConfig(client, resolved.SpriteName); err != nil {
			return fmt.Errorf("push claude config: %w", err)
		}
		return setup.EnsureSpriteClaudeSettings(client, resolved.SpriteName)
	})

	if !exists {
		parallel.Add("Running setup.conf", func() error {
			conf, err := setup.ParseSetupConf(setup.DefaultConfPath())
			if err != nil {
				return fmt.Errorf("parse setup.conf: %w", err)
			}
			if conf != nil {
				setup.RunSetupConf(client, resolved.SpriteName, conf)
			}
			if resolved.LocalPath != "" && !noSync {
				if err := syncInitialFiles(client, resolved.SpriteName, resolved.LocalPath, resolved.RemotePath); err != nil {
					return fmt.Errorf("initial sync: %w", err)
				}
			}
			return nil
		})
	} else {
		parallel.Add("Pushing [always] files", func() error {
			conf, err := setup.ParseSetupConf(setup.DefaultConfPath())
			if err != nil || conf == nil {
				return nil // best-effort
			}
			for _, f := range setup.GetAlwaysFiles(conf) {
				if _, err := os.Stat(f.Source); err == nil {
					client.Exec(sprite.ExecOptions{
						Sprite:  resolved.SpriteName,
						Command: []string{"true"},
						Files:   map[string]string{f.Source: f.Dest},
					})
				}
			}
			return nil
		})
	}

	if err := parallel.Run(); err != nil {
		// Don't fail the whole connect on a single setup-task error —
		// the user probably still wants the shell. Surface the error to
		// stderr (the spinner already showed the failed task) and keep going.
		fmt.Fprintf(os.Stderr, "Warning: setup task failed: %v\n", err)
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
	//
	// Variant sprites (sp . scratch-idea) are deliberately NOT synced after
	// the initial upload: they're throwaway parallel environments, and
	// running a second mutagen session against the same local dir would
	// fight the base sprite's sync. `registerWithDaemon` already strips
	// LocalPath for variants; here we also skip the inline-sync fallback
	// and the "daemon will manage sync" banner.
	if err := registerWithDaemon(resolved, client); err != nil {
		if webMode {
			return fmt.Errorf("daemon required for --web: %w", err)
		}
		fmt.Fprintf(os.Stderr, "Warning: daemon registration failed, sync will not persist: %v\n", err)

		// Fallback: inline sync for console mode when daemon is unavailable.
		// The proxy dies with this process, so it only works while sp is running.
		if !noSync && resolved.LocalPath != "" && resolved.Variant == "" {
			fmt.Println("Starting inline file sync (no daemon)...")
			go func() {
				if err := startSyncInline(client, resolved); err != nil {
					fmt.Fprintf(os.Stderr, "Warning: sync setup failed: %v\n", err)
				}
			}()
		}
	} else if !noSync && resolved.LocalPath != "" && resolved.Variant == "" {
		fmt.Println("Daemon will manage file sync.")
	}

	if resolved.Variant != "" {
		switch {
		case !exists && resolved.LocalPath != "":
			fmt.Printf("Variant %q: initial files uploaded, no ongoing sync. Edit inside the sprite.\n", resolved.Variant)
		case !exists:
			fmt.Printf("Variant %q: fresh sprite, no local sync.\n", resolved.Variant)
		default:
			fmt.Printf("Variant %q: attached (no local sync).\n", resolved.Variant)
		}
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

	// Spawn the keep-warm sentinel BEFORE the foreground exec — once we
	// hand off to execInSprite the current process is essentially blocked
	// on tmux until the user disconnects, and we want the sentinel to
	// already be running so it covers the disconnect window.
	if keepWarmDur > 0 {
		if err := startKeepWarmSentinel(resolved.SpriteName, resolved.Org, keepWarmDur); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: starting keep-warm sentinel: %v\n", err)
		} else if verbose {
			fmt.Fprintf(os.Stderr, "Keep-warm sentinel started (max %s, exits early when claude is idle for 60s)\n", keepWarmDur)
		}
	}

	// Connect to sprite shell. Pass authTokenForEnv (empty when we pushed
	// a credentials.json) so execInSprite knows whether to inject the
	// CLAUDE_CODE_OAUTH_TOKEN env var / tmux setenv. Injecting it
	// alongside credentials.json would mask the file and defeat
	// auto-refresh.
	return execInSprite(client, resolved, authTokenForEnv)
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

// startKeepWarmSentinel spawns a backgrounded sprite-exec process that
// runs a watcher script on the sprite. The watcher hashes the active
// tmux pane every 10 seconds; when the hash is stable for 60s the
// watcher assumes claude (or whatever is in the foreground) has
// reached a prompt/idle state and exits. Otherwise it runs until the
// duration deadline.
//
// The mechanism that keeps the sprite warm is the sprite-exec connection
// itself: as long as a sprite-exec process is talking to the sprite,
// sprite-env counts it as an active client and the underlying Fly
// machine doesn't auto-stop. When the watcher script exits, the
// sprite-exec connection closes and (assuming no other clients) the
// machine eventually cold-stops normally.
//
// The local sprite-exec process is detached from sp's process group
// via Setpgid + redirected stdio so it survives the foreground sp
// process exiting (e.g. when the user closes their tmux session).
//
// Idempotent: any prior keep-warm watcher on the same sprite is killed
// at the start of the script via pkill against an embedded marker, so
// reconnects don't accumulate watchers.
func startKeepWarmSentinel(spriteName, org string, dur time.Duration) error {
	if dur <= 0 {
		return nil
	}
	deadline := time.Now().Add(dur).Unix()

	// Marker is grep'd by pkill to identify our watchers across sp runs.
	// Embedded as a comment in the script body so it appears in the
	// process command line that pkill -f matches against.
	const marker = "SP_KEEPWARM_MARKER_v1"
	const idleTicks = 6 // 6 * 10s = 60s of pane stability before declaring idle
	script := fmt.Sprintf(`
# %s
# Kill any existing keep-warm watchers from prior sp runs.
pkill -f '%s' 2>/dev/null || true
DEADLINE=%d
LAST_HASH=""
IDLE_COUNT=0
while [ "$(date +%%s)" -lt "$DEADLINE" ]; do
    HASH=$(tmux capture-pane -t bash -p 2>/dev/null | sha256sum | cut -d' ' -f1)
    if [ "$HASH" = "$LAST_HASH" ]; then
        IDLE_COUNT=$((IDLE_COUNT + 1))
        if [ "$IDLE_COUNT" -ge %d ]; then
            exit 0
        fi
    else
        IDLE_COUNT=0
    fi
    LAST_HASH="$HASH"
    sleep 10
done
`, marker, marker, deadline, idleTicks)

	args := []string{"exec"}
	if org != "" {
		args = append(args, "-o", org)
	}
	args = append(args, "-s", spriteName, "sh", "-c", script)

	cmd := exec.Command("sprite", args...)
	// Setpgid puts the child in its own process group so SIGHUP from sp's
	// controlling terminal at exit doesn't propagate to it. Redirected
	// stdio severs any inherited ttys.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting keep-warm sentinel: %w", err)
	}
	// Release so the runtime doesn't track the child after we leave the
	// function. The child becomes a zombie when it exits, eventually
	// reaped by init. We don't care about its exit code.
	if err := cmd.Process.Release(); err != nil {
		return fmt.Errorf("releasing keep-warm sentinel: %w", err)
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
		// Previously printed a `.` per retry, but the spinner now provides
		// progress indication so the dots would interleave with the live
		// render. Verbose mode could re-add this if useful, but for now
		// the spinner duration counter suffices.
	}
	return fmt.Errorf("sprite did not become ready within 60 seconds")
}

// cloneRepoOnSprite runs `git clone` inside the sprite for the given GitHub
// owner/repo. Uses the SSH URL so the deployed key from SetupSpriteAuth is
// picked up (SetupGitConfig also rewrites HTTPS GitHub URLs to SSH as a
// safety net). Handles three pre-states of the target directory:
//
//   - doesn't exist: git clone creates it
//   - exists and is already a git repo: skipped (idempotent)
//   - exists and is empty: rmdir'd first so git clone has a clean slate —
//     git *does* allow cloning into an existing empty dir, but removing it
//     first avoids any subtle differences in git version behavior
//   - exists and is non-empty and not a repo: error with a clear message
//
// All four states are handled in a single shell script so we only pay one
// round trip to the sprite.
func cloneRepoOnSprite(client *sprite.Client, spriteName, ownerRepo, remoteDir string) error {
	sshURL := fmt.Sprintf("git@github.com:%s.git", ownerRepo)
	parent, _ := splitRemoteDir(remoteDir)
	q := shellQuote
	// Consolidated pre-check + clone. Exit codes:
	//   0   success (cloned or already present)
	//   2   target exists as non-repo with content (user must resolve)
	//   3   git clone itself failed
	script := fmt.Sprintf(`
set -e
DIR=%s
PARENT=%s
URL=%s

if [ -d "$DIR/.git" ]; then
  echo "sp: repo already present at $DIR"
  exit 0
fi

if [ -d "$DIR" ]; then
  if [ -z "$(ls -A "$DIR" 2>/dev/null)" ]; then
    echo "sp: removing empty $DIR so git clone can create it"
    rmdir "$DIR"
  else
    echo "sp: $DIR exists and is not empty and is not a git repo — refusing to clobber" >&2
    ls -la "$DIR" >&2
    exit 2
  fi
fi

mkdir -p "$PARENT"
cd "$PARENT"
echo "sp: cloning $URL into $DIR"
git clone --recurse-submodules "$URL" "$(basename "$DIR")" || exit 3
`, q(remoteDir), q(parent), q(sshURL))

	// GH_TOKEN must be in the exec env so the git credential helper
	// (installed by SetupGitConfig) can authenticate HTTPS requests.
	// The URL rewrite converts git@github.com: to https://github.com/,
	// so the clone goes over HTTPS and needs the token.
	env := map[string]string{}
	if ghToken := setup.LocalGhToken(); ghToken != "" {
		env["GH_TOKEN"] = ghToken
	}

	out, err := client.Exec(sprite.ExecOptions{
		Sprite:  spriteName,
		Env:     env,
		Command: []string{"sh", "-c", script},
	})
	// In verbose mode, surface the script's status output ("sp: cloning…",
	// "sp: repo already present", etc.) so users can see what happened.
	// In spinner mode the print would interleave with the live frame, so
	// the spinner's task name + duration carries the signal instead.
	if verbose {
		if trimmed := strings.TrimSpace(string(out)); trimmed != "" {
			fmt.Fprintln(os.Stderr, trimmed)
		}
	}
	if err != nil {
		return fmt.Errorf("clone script: %w\n%s", err, string(out))
	}
	return nil
}

// splitRemoteDir splits a remote dir into its parent and last segment.
// "/home/sprite/gameservers" -> ("/home/sprite", "gameservers"). Defaults
// to ("/home/sprite", "repo") if the path is malformed.
func splitRemoteDir(p string) (parent, target string) {
	p = strings.TrimRight(p, "/")
	idx := strings.LastIndex(p, "/")
	if idx <= 0 {
		return "/home/sprite", "repo"
	}
	return p[:idx], p[idx+1:]
}

// shellQuote wraps a string in single quotes for safe interpolation into a
// sh -c command string, escaping embedded single quotes.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
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

	// Variant sprites with a local path get an initial one-shot upload at
	// creation (see runConnect) and then run unsynced — they should NOT be
	// registered with a LocalPath, or the daemon will start an ongoing mutagen
	// session that conflicts with the base sprite's sync. Storing the base's
	// LocalPath on a variant would also give the daemon a sibling to watch.
	localPath := resolved.LocalPath
	if resolved.Variant != "" {
		localPath = ""
	}

	s := &store.Sprite{
		Name:       resolved.SpriteName,
		LocalPath:  localPath,
		RemotePath: resolved.RemotePath,
		Repo:       resolved.Repo,
		Org:        resolved.Org,
		Variant:    resolved.Variant,
		BaseName:   resolved.BaseName,
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
	mutagenID, err := mgr.StartMutagenSession(resolved.SpriteName, resolved.LocalPath, resolved.RemotePath, "")
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
// execInSprite connects to the sprite with a tmux session. On the first
// connect a new sprite-exec persistent session is created (TTY sessions
// are kept alive by sprite-env even after the client disconnects). On
// subsequent connects we detect the existing session and reattach via
// `sprite attach`, which means tmux, claude, and anything else running
// inside the session survive network drops and reconnects.
func execInSprite(client *sprite.Client, resolved *setup.ResolvedTarget, token string) error {
	// Check for an existing persistent session from a prior connect.
	// If found, reattach directly — tmux + claude are still running.
	if sessionID := findExistingSpriteSession(resolved.SpriteName, resolved.Org); sessionID != "" {
		fmt.Printf("Reattaching to session %s...\n", sessionID)
		return attachToSpriteSession(client, resolved.SpriteName, resolved.Org, sessionID)
	}

	// No existing session — create a new one with the full tmux setup.
	return createSpriteSession(client, resolved, token)
}

// findExistingSpriteSession queries sprite sessions list and returns the
// ID of the first active tmux session, or "" if none. Only matches
// sessions whose Command column contains "tmux" to avoid accidentally
// reattaching to stale non-interactive exec sessions (e.g. leftover
// setup commands like FixSpriteHomePermissions that sprite-env keeps
// around as "Active" sessions).
func findExistingSpriteSession(spriteName, org string) string {
	args := []string{"sessions", "list"}
	if org != "" {
		args = append(args, "-o", org)
	}
	args = append(args, "-s", spriteName)

	out, err := exec.Command("sprite", args...).CombinedOutput()
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		// First field must be a numeric session ID.
		if _, err := fmt.Sscanf(fields[0], "%d", new(int)); err != nil {
			continue
		}
		// Only match tmux sessions — the Command column (field 1+)
		// should contain "tmux". This filters out stale setup exec
		// sessions (chown, git config, etc.) that sprite-env keeps alive.
		rest := strings.Join(fields[1:], " ")
		if strings.Contains(rest, "tmux") {
			return fields[0]
		}
	}
	return ""
}

// buildTmuxEnvRefreshScript builds a shell snippet that updates the
// given vars in BOTH the tmux global environment AND every existing
// session's environment. Updating only `-g` (global) is insufficient
// after a session exists: on session creation tmux snapshots the
// global env into the session env, and new panes/windows in that
// session inherit from the session env — not from global. So a
// post-create `setenv -g` never reaches new panes in the live session.
//
// To reach new panes in the existing tmux session, we iterate
// `tmux list-sessions` and call `setenv -t <session>` for each one.
// We also still set `-g` so any session created later picks up the
// fresh value at its snapshot moment.
//
// Returns "" when vars is empty so callers can short-circuit.
func buildTmuxEnvRefreshScript(vars map[string]string) string {
	if len(vars) == 0 {
		return ""
	}
	var sets []string
	for k, v := range vars {
		sets = append(sets, fmt.Sprintf("tmux setenv -g %s %s 2>/dev/null || true", k, shellQuote(v)))
		sets = append(sets, fmt.Sprintf(
			`for s in $(tmux list-sessions -F '#{session_name}' 2>/dev/null); do tmux setenv -t "$s" %s %s 2>/dev/null || true; done`,
			k, shellQuote(v),
		))
	}
	return strings.Join(sets, "\n")
}

// attachToSpriteSession reattaches to an existing sprite-env session by
// its numeric ID via `sprite attach`. Before attaching, it refreshes
// the tmux global environment with the current GH_TOKEN and
// CLAUDE_CODE_OAUTH_TOKEN so new panes pick up fresh tokens (since we
// no longer persist tokens in rc files).
func attachToSpriteSession(client *sprite.Client, spriteName, org, sessionID string) error {
	// Refresh tmux env vars before reattaching. Tokens are no longer in
	// rc files (security: we don't leave tokens on the dormant filesystem),
	// so they must be injected into the tmux server's global env on every
	// connect. Without this, panes opened after a reconnect would have no
	// token and the relevant tools (git, claude) would fail.
	//
	// Note: existing panes inside the session keep their old env — only
	// new panes/windows inherit the refreshed value. That's a tmux
	// limitation, not something we can work around here.
	vars := map[string]string{}
	if ghToken := setup.LocalGhToken(); ghToken != "" {
		vars["GH_TOKEN"] = ghToken
	}
	if claudeToken, err := setup.NewTokenProvider().LocalToken(); err == nil && claudeToken != "" {
		vars["CLAUDE_CODE_OAUTH_TOKEN"] = claudeToken
	}
	if script := buildTmuxEnvRefreshScript(vars); script != "" {
		client.Exec(sprite.ExecOptions{
			Sprite:  spriteName,
			Command: []string{"sh", "-c", script},
		})
	}

	args := []string{"attach", sessionID}
	if org != "" {
		args = append(args, "-o", org)
	}
	args = append(args, "-s", spriteName)

	binary, err := exec.LookPath("sprite")
	if err != nil {
		return fmt.Errorf("sprite binary not found: %w", err)
	}

	cmd := exec.Command(binary, args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	runErr := cmd.Run()
	resetTerminal()
	return runErr
}

// createSpriteSession creates a brand-new sprite-exec persistent session
// running tmux. Sprite-env keeps TTY sessions alive even after the
// client disconnects, so the tmux server (and claude inside it) survive
// network drops. On the next connect, execInSprite will detect the
// existing session via findExistingSpriteSession and reattach.
func createSpriteSession(client *sprite.Client, resolved *setup.ResolvedTarget, token string) error {
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
		if parts := strings.Fields(tmuxSession); len(parts) > 0 {
			tmuxSession = parts[0]
		}
	}

	// Ensure the target dir exists
	if _, err := client.Exec(sprite.ExecOptions{
		Sprite:  resolved.SpriteName,
		Command: []string{"mkdir", "-p", resolved.RemotePath},
	}); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: mkdir -p %s failed: %v\n", resolved.RemotePath, err)
	}

	// Build the per-tool tmux setenv lines. Uses the shared helper so
	// both -g (for newly-created sessions) and -t <session> (for any
	// session that already exists — usually only on a reconnect race)
	// are updated. When there's no token to set, fall through to the
	// explicit -gu unset to clear stale values.
	ghToken := setup.LocalGhToken()
	refreshVars := map[string]string{}
	if token != "" {
		refreshVars["CLAUDE_CODE_OAUTH_TOKEN"] = token
	}
	if ghToken != "" {
		refreshVars["GH_TOKEN"] = ghToken
	}
	tokenRefresh := buildTmuxEnvRefreshScript(refreshVars)
	if tokenRefresh != "" {
		tokenRefresh += "\n"
	}
	if token == "" {
		tokenRefresh = "tmux setenv -gu CLAUDE_CODE_OAUTH_TOKEN 2>/dev/null || true\n" + tokenRefresh
	}

	shellCmd := fmt.Sprintf(`
# Start ssh-agent if not already running. The agent lives in the tmux
# server's process tree, so it survives reconnects (persistent session).
# The user runs 'ssh-add' once per session to unlock passphrase-protected
# SSH keys; all subsequent SSH operations (scp, ssh to other hosts) use
# the agent. Git operations don't need the agent at all — they use
# HTTPS + GH_TOKEN via the credential helper set up by SetupGitConfig.
if [ -z "$SSH_AUTH_SOCK" ]; then
  eval "$(ssh-agent -s)" > /dev/null 2>&1
fi

tmux start-server 2>/dev/null || true
tmux set -g allow-passthrough on 2>/dev/null || true

# Pass SSH_AUTH_SOCK into tmux so new panes inherit the agent.
if [ -n "$SSH_AUTH_SOCK" ]; then
  tmux setenv -g SSH_AUTH_SOCK "$SSH_AUTH_SOCK" 2>/dev/null || true
fi

for v in ANTHROPIC_API_KEY ANTHROPIC_AUTH_TOKEN CLAUDE_CODE_USE_BEDROCK CLAUDE_CODE_USE_VERTEX CLAUDE_CODE_USE_FOUNDRY; do
  tmux setenv -gu "$v" 2>/dev/null || true
done
%sexec tmux new-session -A -s %s -c %s %s
`, tokenRefresh, shellQuote(tmuxSession), shellQuote(resolved.RemotePath), command)

	env := map[string]string{}
	if token != "" {
		env["CLAUDE_CODE_OAUTH_TOKEN"] = token
	}
	if ghToken != "" {
		env["GH_TOKEN"] = ghToken
	}

	args := client.BuildExecArgs(sprite.ExecOptions{
		Sprite:  resolved.SpriteName,
		TTY:     true,
		Dir:     resolved.RemotePath,
		Env:     env,
		Command: []string{"sh", "-c", shellCmd},
	})

	binary, err := exec.LookPath("sprite")
	if err != nil {
		return fmt.Errorf("sprite binary not found: %w", err)
	}

	cmd := exec.Command(binary, args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	runErr := cmd.Run()
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
