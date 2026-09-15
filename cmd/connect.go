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
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/sys/unix"

	"github.com/jphenow/sp/internal/daemon"
	"github.com/jphenow/sp/internal/progress"
	"github.com/jphenow/sp/internal/setup"
	"github.com/jphenow/sp/internal/sprite"
	"github.com/jphenow/sp/internal/store"
	spSync "github.com/jphenow/sp/internal/sync"
)

var (
	noSync        bool
	sessionName   string
	webMode       bool
	webProxy      bool
	webDevPort    int
	execCmd       string
	keepWarmDur   time.Duration
	remoteControl bool
	rcAlias       bool
	noHold        bool
)

// holdCap bounds the default session-tied hold: the sprite is held Active while
// its tmux session lives, but never past this cap, so a session left open
// overnight can't bill indefinitely. --keep-warm replaces the hold with the
// older idle-exit behavior; --no-hold disables it entirely.
const holdCap = 8 * time.Hour

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
	connectCmd.Flags().DurationVar(&keepWarmDur, "keep-warm", 0, "hold the sprite Active (Tasks API) for up to this duration after disconnect, exiting early if claude is idle for 60s (e.g. 1h, 30m). Default off. See also 'sp keepalive'.")
	connectCmd.Flags().BoolVar(&remoteControl, "remote-control", false, "launch Claude with Remote Control so the session can be joined from your phone/browser (claude.ai/code)")
	connectCmd.Flags().BoolVar(&rcAlias, "rc", false, "alias for --remote-control")
	connectCmd.Flags().BoolVar(&noHold, "no-hold", false, "don't hold the sprite Active for the life of the session (it may idle-pause while you're away)")

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

	// Claude auth inputs. BOTH are gathered every time, because they serve
	// different goals and neither alone covers both:
	//
	//   - credentials.json (full-scope claude.ai login) is what Remote
	//     Control requires; a setup-token is inference-only and RC rejects it.
	//   - ~/.claude-token (setup-token) is long-lived and never rotates, so
	//     it's the reliable fallback that keeps plain inference working when
	//     the shared credential has been invalidated by refresh-token
	//     rotation elsewhere.
	//
	// Which one the sprite actually ends up using is decided AFTER the push,
	// by asking claude on the sprite whether the credential works — see the
	// auth chain below. LocalToken is the non-prompting read; only fall back
	// to the interactive prompt when there's no other credential at all.
	creds := setup.LocalClaudeCredentials()
	tp := setup.NewTokenProvider()
	token, _ := tp.LocalToken()
	if creds == nil && token == "" {
		var err error
		token, err = tp.GetToken()
		if err != nil {
			return fmt.Errorf("getting token: %w", err)
		}
	}

	// Create sprite client
	client := sprite.NewClient(resolved.Org)

	// Check if sprite exists, create if needed. ExistsInfo hands back the Info
	// the existence check already fetched, so the lifecycle status
	// (running/warm/cold) costs nothing extra — and it's the single most
	// useful number for explaining a slow connect, since a cold sprite makes
	// the first exec pay a ~30s wake that every later call avoids.
	connectStart := time.Now()
	info, exists, err := client.ExistsInfo(resolved.SpriteName)
	if err != nil {
		return fmt.Errorf("checking sprite: %w", err)
	}
	startStatus := "new"
	if exists && info != nil && info.Status != "" {
		startStatus = info.Status
	}

	// Sequential prologue: create + wait + perms must happen in order before
	// any parallel setup (earlier bug: a parallel perms-fix raced with the
	// ready-check and waited 55s instead of piggybacking). These used to be
	// hidden inside ONE task named "Preparing sprite", which meant a slow
	// prologue was unattributable — you couldn't tell a slow create from a
	// slow wake from a slow chown. RunSequential keeps the ordering and gives
	// each step its own timed line.
	prologue := progress.New(verbose)
	prologue.SetFooter(inFlightFooter)
	if !exists {
		prologue.Add("Creating sprite", func() error {
			if err := client.Create(resolved.SpriteName); err != nil {
				return fmt.Errorf("creating sprite: %w", err)
			}
			return nil
		})
	}
	// The readiness probe doubles as the home-permissions fix: both are one
	// trivial command, and a call's cost is almost entirely the dial, so
	// merging them saves a full round trip on every connect.
	var readyTask *progress.Task
	readyTask = prologue.Add("Waiting for sprite", func() error {
		return waitForSpriteReady(client, resolved.SpriteName, readyTask)
	})
	if err := prologue.RunSequential(); err != nil {
		return err
	}

	// authTokenForEnv is set by the auth chain below once we know whether the
	// sprite's credentials.json actually authenticates. Empty means "don't
	// inject CLAUDE_CODE_OAUTH_TOKEN" — the env var sits ABOVE the
	// credentials file in claude's auth precedence, so injecting it masks the
	// file and breaks Remote Control (and makes an on-sprite /login look like
	// it didn't stick).
	var authTokenForEnv string

	// Remote Control: default the command to Claude with the join feature on, so
	// the session can be driven from claude.ai/code or the Claude app.
	if rcAlias {
		remoteControl = true
	}
	if remoteControl && execCmd == "" {
		execCmd = "claude --remote-control"
	}

	// Parallel setup. Five concurrent task chains:
	//
	//   1. Auth chain (SetupSpriteAuth → SyncClaudeCredentials →
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
	parallel.SetFooter(inFlightFooter)

	parallel.Add("Setting up auth + gh + clone", func() error {
		// Auth must complete before clone because the clone needs the SSH
		// key and StrictHostKeyChecking=no config that auth deploys. These
		// are chained in one parallel task so other tasks (config push,
		// git config) run concurrently while this chain serializes.
		if err := setup.SetupSpriteAuth(client, resolved.SpriteName); err != nil {
			return fmt.Errorf("sprite auth: %w", err)
		}
		// Install the credential (only if fresher than the sprite's) and find
		// out whether the sprite ends up authenticated — one call, not three.
		//
		// Decide the env-token question with evidence rather than inference: a
		// credentials.json that exists but doesn't work (refresh token rotated
		// out from under it) must NOT suppress the setup-token fallback, or the
		// sprite lands on "Not logged in" with nothing to fall back to. Written
		// here and read after parallel.Run() joins.
		fullyAuthed, authMethod := setup.SyncClaudeCredentials(client, resolved.SpriteName, creds)
		switch {
		case fullyAuthed:
			authTokenForEnv = "" // full-scope creds work; don't mask them
		case token != "":
			authTokenForEnv = token
			progress.Warnf("Note: the sprite has no working claude.ai login (auth method: %q), so falling back to the inference-only setup-token. Inference will work; Remote Control will not until you run /login on the sprite.", authMethod)
		default:
			progress.Warnf("Warning: no working Claude credential on the sprite and no ~/.claude-token to fall back on. Run 'claude' then /login on the sprite.")
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
		// Non-fatal: this is preferences, skills and commands, not something
		// the session needs to start. A flaky upload here shouldn't cost you
		// the connect — warn and carry on to the settings merge, which is
		// what actually makes claude usable (bypass-permissions defaults).
		if err := setup.PushClaudeConfig(client, resolved.SpriteName); err != nil {
			progress.Warnf("Warning: Claude config not pushed (your skills/commands may be missing on the sprite): %v", err)
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
			// One exec for the whole set: each upload against a cold sprite
			// can take 30s+, and doing them serially is what pushed this past
			// the CLI's upload timeout in the first place.
			files := map[string]string{}
			for _, f := range setup.GetAlwaysFiles(conf) {
				if _, err := os.Stat(f.Source); err == nil {
					files[f.Source] = f.Dest
				}
			}
			// Report failures instead of discarding them. This task used to
			// throw away every error, so a batch of timed-out uploads still
			// printed a green check and the user found a sprite with none of
			// their dotfiles on it and no clue why.
			return setup.UploadFiles(client, resolved.SpriteName, files)
		})
	}

	if err := parallel.Run(); err != nil {
		// Don't fail the whole connect on a single setup-task error — the
		// user probably still wants the shell. Report EVERY failure, not
		// just the first Run() returns: during an API outage several tasks
		// fail together, and the progress line truncates each error to fit
		// the terminal, so this is where the full text has to come out.
		for _, t := range parallel.Failures() {
			fmt.Fprintf(os.Stderr, "\nWarning: setup task %q failed: %v\n", t.Name, t.Err)
		}
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
	} else if !noHold {
		// Default: hold the sprite Active while its tmux session lives. This is
		// deliberately not tied to --rc, because you don't decide up front
		// whether you'll switch to the phone — you may run /remote-control from
		// inside an already-running session, and that only works if the sprite
		// hasn't paused in the meantime. It also keeps a plain terminal session
		// from dropping its connection on an idle pause.
		//
		// Idle-exit is wrong here (it assumes idle == done, but an idle prompt
		// usually means you stepped away), so the hold releases on session end
		// instead — or at holdCap, so an abandoned session can't bill forever.
		if err := launchKeepAlive(resolved.SpriteName, resolved.Org, holdCap, "", deriveTmuxSessionName()); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: starting session hold: %v\n", err)
		} else if verbose {
			fmt.Fprintf(os.Stderr, "Holding sprite Active until this session ends (cap %s)\n", holdCap)
		}
	}

	reportConnectTimings(resolved.SpriteName, startStatus, time.Since(connectStart))

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

// startKeepWarmSentinel holds the sprite Active (via the Tasks API heartbeat in
// launchKeepAlive) for up to dur, exiting early once the tmux session has been
// idle for ~60s. This replaces the older pane-hash-holds-a-connection hack: the
// Tasks API keeps the sprite in the Active state directly, so the session's
// connection doesn't drop and processes don't cold-die while you're working.
// Idle-exit still stops the hold (and billing) when Claude reaches a prompt.
func startKeepWarmSentinel(spriteName, org string, dur time.Duration) error {
	if dur <= 0 {
		return nil
	}
	return launchKeepAlive(spriteName, org, dur, deriveTmuxSessionName(), "")
}

// waitForSpriteReady polls until the sprite responds to commands.
// inFlightMinAge is how long a sprite call must be running before it shows up
// in the progress footer. Short enough to catch a stall early, long enough that
// the healthy case (calls returning in ~0.2s) never flickers.
const inFlightMinAge = 3 * time.Second

// inFlightFooter reports the sprite CLI calls currently in flight, so a task
// that appears frozen names the call it's waiting on.
func inFlightFooter() []string {
	return sprite.InFlightSummary(inFlightMinAge)
}

// slowConnectThreshold is the setup duration above which the latency profile
// prints unasked. Below it the numbers are noise; above it they're the first
// thing you want, and a slow connect is exactly the run you can't reproduce on
// demand with a flag.
const slowConnectThreshold = 45 * time.Second

// reportConnectTimings prints where the setup phase's wall clock went: the
// sprite's lifecycle status when we started (a cold sprite explains ~30s of
// wake on its own), total elapsed, and the per-call profile from the sprite
// trace. Prints under -v always, and unprompted when setup ran long.
func reportConnectTimings(spriteName, startStatus string, elapsed time.Duration) {
	slow := elapsed >= slowConnectThreshold
	if !verbose && !slow {
		return
	}
	calls, busy := sprite.TotalTraced()
	fmt.Fprintf(os.Stderr, "\nSetup took %s — sprite %q was %q on connect; %d sprite CLI calls, %s of call time.\n",
		elapsed.Round(time.Millisecond), spriteName, startStatus, calls, busy.Round(time.Millisecond))
	if slow && startStatus == "cold" {
		fmt.Fprintln(os.Stderr, "A cold sprite pays its wake on the first call (~30s observed); later calls run in well under a second.")
	}
	if summary := sprite.TraceSummary(); summary != "" {
		fmt.Fprint(os.Stderr, summary)
	}
}

// spriteReadyTimeout bounds waitForSpriteReady in wall-clock time. Sized
// against a measured ~31s cold-start wake: enough room for a few of those,
// short enough that a genuinely broken sprite doesn't hang the connect.
const spriteReadyTimeout = 3 * time.Minute

// waitForSpriteReadyMaxBackoff caps the sleep between probe attempts.
const waitForSpriteReadyMaxBackoff = 8 * time.Second

// waitForSpriteReady polls the sprite with a trivial exec until it answers.
//
// The probe itself is the wake: on a cold sprite the first exec blocks for the
// full cold-start (~31s measured) before returning, so an attempt here is not
// cheap and must not be retried in a tight loop.
//
// This replaces a loop that ran a fixed 60 iterations with no sleep, reported
// "did not become ready within 60 seconds" regardless of how long it actually
// ran (60 × a 31s cold exec is over half an hour), and discarded every error —
// making it impossible to tell a slow sprite from a broken one. progress
// reports live attempt/elapsed state, and the last error survives into the
// returned message.
func waitForSpriteReady(client *sprite.Client, name string, task *progress.Task) error {
	deadline := time.Now().Add(spriteReadyTimeout)
	backoff := time.Second
	var lastErr error

	for attempt := 1; ; attempt++ {
		if task != nil {
			task.SetDetail(fmt.Sprintf("probe %d", attempt))
		}
		started := time.Now()
		_, err := client.Exec(sprite.ExecOptions{
			Sprite:  name,
			Command: []string{"sh", "-c", setup.HomePermissionsScript},
		})
		if err == nil {
			if task != nil && attempt > 1 {
				task.SetDetail(fmt.Sprintf("ready after %d probes", attempt))
			} else if task != nil {
				task.SetDetail("")
			}
			return nil
		}
		lastErr = err
		if task != nil {
			task.SetDetail(fmt.Sprintf("probe %d failed after %s, retrying",
				attempt, time.Since(started).Round(time.Second)))
		}

		if time.Now().After(deadline) {
			return fmt.Errorf("sprite %q not ready after %s (%d probes); last error: %w",
				name, spriteReadyTimeout, attempt, lastErr)
		}
		time.Sleep(backoff)
		if backoff *= 2; backoff > waitForSpriteReadyMaxBackoff {
			backoff = waitForSpriteReadyMaxBackoff
		}
	}
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
	// Forward Claude /login callback ports for the life of the attach, so the
	// browser login completes on its own instead of asking for a pasted code.
	stopLoginForwarder := startLoginCallbackForwarder(client, resolved.SpriteName, resolved.Org)
	defer stopLoginForwarder()

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
// setup commands like the home-permissions probe that sprite-env keeps
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

// buildTmuxEnvUnsetScript is the mirror image of buildTmuxEnvRefreshScript:
// it removes the named vars from the tmux global environment AND from every
// existing session's environment. The per-session sweep matters as much here
// as it does for setting — `tmux new-session -A` attaches to a live session
// whose env was snapshotted from global at creation time, so a `setenv -gu`
// alone leaves the stale value reachable by every new pane.
func buildTmuxEnvUnsetScript(names []string) string {
	if len(names) == 0 {
		return ""
	}
	var unsets []string
	for _, k := range names {
		unsets = append(unsets, fmt.Sprintf("tmux setenv -gu %s 2>/dev/null || true", k))
		unsets = append(unsets, fmt.Sprintf(
			`for s in $(tmux list-sessions -F '#{session_name}' 2>/dev/null); do tmux setenv -t "$s" -u %s 2>/dev/null || true; done`,
			k,
		))
	}
	return strings.Join(unsets, "\n")
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

	// Claude auth on reattach mirrors the connect path: push the local
	// full-scope credential when it's fresher than the sprite's (a sprite
	// that's been cold for a while can hold an expired one, which would
	// otherwise surface as a /login prompt), and inject the setup-token env
	// var ONLY when no credentials.json exists anywhere. Injecting it
	// unconditionally — as this used to — re-masks the sprite's own
	// credential on every single reconnect, which is why a /login performed
	// on the sprite never survived to the next session.
	var unset []string
	claudeToken, _ := setup.NewTokenProvider().LocalToken()
	if fullyAuthed, _ := setup.SyncClaudeCredentials(client, spriteName, setup.LocalClaudeCredentials()); fullyAuthed {
		unset = append(unset, "CLAUDE_CODE_OAUTH_TOKEN")
	} else if claudeToken != "" {
		vars["CLAUDE_CODE_OAUTH_TOKEN"] = claudeToken
	}

	script := strings.TrimSpace(buildTmuxEnvUnsetScript(unset) + "\n" + buildTmuxEnvRefreshScript(vars))
	if script != "" {
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

	runErr := runAttachWithRetry(binary, args)
	resetTerminal()
	return runErr
}

// attachFailFastWindow bounds how quickly an attach must fail to be considered
// "never got going". Past this, the user had a real session and an exit is
// theirs, not a connection error to paper over.
const attachFailFastWindow = 10 * time.Second

// runAttachWithRetry runs `sprite attach`, retrying once if it dies almost
// immediately.
//
// Setup can take minutes against a degraded API, and losing all of it to a
// connection error on the very last step is the worst possible outcome —
// observed: six and a half minutes of successful setup, then "failed to
// connect: read tcp ...: i/o timeout" and exit 1. The dial is the flaky part
// (see the trace: essentially all of a call's time is spent connecting), and a
// dial that fails fast is exactly the case a retry fixes.
//
// Only fast failures are retried. Once the attach has been up long enough to
// hand the terminal over, a non-zero exit means the user's session ended and
// re-running it would be wrong.
func runAttachWithRetry(binary string, args []string) error {
	for attempt := 1; ; attempt++ {
		cmd := exec.Command(binary, args...)
		cmd.Stdin = os.Stdin
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr

		start := time.Now()
		err := cmd.Run()
		if err == nil || attempt > 1 || time.Since(start) > attachFailFastWindow {
			return err
		}
		fmt.Fprintf(os.Stderr, "\nAttach failed after %s (%v) — retrying once.\n",
			time.Since(start).Round(time.Millisecond), err)
	}
}

// deriveTmuxSessionName computes the tmux session name a connect will use:
// the explicit --name if set, otherwise the first word of the command (bash by
// default), with characters that are awkward in tmux names replaced by dashes.
// Shared by createSpriteSession (which creates it) and the keep-warm heartbeat
// (which watches it for idle-exit), so they always agree on the name.
func deriveTmuxSessionName() string {
	if sessionName != "" {
		return sessionName
	}
	name := "bash"
	if execCmd != "" {
		name = execCmd
	}
	name = strings.ReplaceAll(name, " ", "-")
	name = strings.ReplaceAll(name, ".", "-")
	name = strings.ReplaceAll(name, ":", "-")
	if parts := strings.Fields(name); len(parts) > 0 {
		name = parts[0]
	}
	return name
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

	tmuxSession := deriveTmuxSessionName()

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
		// Clear the var everywhere, not just globally: `new-session -A` can
		// attach to a pre-existing session that snapshotted an old value, and
		// a lingering CLAUDE_CODE_OAUTH_TOKEN masks ~/.claude/.credentials.json
		// for every pane opened in it.
		tokenRefresh = buildTmuxEnvUnsetScript([]string{"CLAUDE_CODE_OAUTH_TOKEN"}) + "\n" + tokenRefresh
	}
	// Remote Control: give the session a readable name prefix so the sprite is
	// easy to find in the claude.ai/code session list (defaults to hostname).
	rcSetenv := ""
	if remoteControl {
		rcSetenv = fmt.Sprintf("tmux setenv -g CLAUDE_REMOTE_CONTROL_SESSION_NAME_PREFIX %s 2>/dev/null || true\n", shellQuote(resolved.SpriteName))
	}

	// Work around the sprite CLI's --tty not propagating the initial window
	// size to the remote PTY: it comes up 0x0, which makes tmux and
	// full-screen TUIs (claude) render one column off. Seed the PTY size
	// from the local terminal before tmux starts so it lays out correctly.
	// Caveat: this fixes only the *initial* size — the CLI also doesn't
	// forward live SIGWINCH, so resizing the local window mid-session won't
	// reflow until tmux is detached and reattached.
	sttyPrefix := ""
	if rows, cols, ok := localTerminalSize(); ok {
		sttyPrefix = fmt.Sprintf("stty rows %d cols %d 2>/dev/null || true\n", rows, cols)
	}

	shellCmd := fmt.Sprintf(`%s
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
%s%sexec tmux new-session -A -s %s -c %s %s
`, sttyPrefix, tokenRefresh, rcSetenv, shellQuote(tmuxSession), shellQuote(resolved.RemotePath), command)

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

// localTerminalSize returns the local controlling terminal's size (rows,
// cols) by probing stdout/stdin/stderr in turn. ok is false when none is a
// TTY (e.g. output is piped), in which case callers should skip the stty
// seed and let the remote default stand.
func localTerminalSize() (rows, cols int, ok bool) {
	for _, f := range []*os.File{os.Stdout, os.Stdin, os.Stderr} {
		ws, err := unix.IoctlGetWinsize(int(f.Fd()), unix.TIOCGWINSZ)
		if err == nil && ws.Row > 0 && ws.Col > 0 {
			return int(ws.Row), int(ws.Col), true
		}
	}
	return 0, 0, false
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
