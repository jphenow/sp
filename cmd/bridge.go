package cmd

import (
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/spf13/cobra"

	"github.com/jphenow/sp/internal/keepalive"
	"github.com/jphenow/sp/internal/sprite"
)

// A bridge makes a sleeping sprite look like an always-on server to a client
// that knows nothing about sprites.
//
// The case this exists for is T3 Code on a phone. Its mobile client reaches a
// backend by address and pairing code — there is no SSH option and no hook to
// run anything before connecting, so it cannot wake a sprite itself, and a
// sprite that has paused looks to it like a dead host. Pointing it straight at
// the sprite's own URL doesn't work either: that URL is behind org OAuth, which
// a native app can't complete, and making it public would expose every service
// on the sprite.
//
// So the bridge runs on a box that stays up. It listens locally, and the first
// byte of a connection is what wakes the sprite — the client just sees a slow
// first connect. While anything is connected the sprite is held Active, because
// services alone don't stop it pausing and a mid-session pause would drop the
// client.
//
// Each connection gets its own `sprite proxy --stdio` pipe rather than sharing
// one forwarded port, which keeps connection lifetimes independent and means
// nothing has to allocate or clean up local ports.

const (
	// bridgeIdleGrace is how long the hold outlives the last connection.
	// Clients reconnect routinely — a phone changing networks, a screen
	// locking — and releasing the hold instantly would pause the sprite
	// between two halves of one session.
	bridgeIdleGrace = 3 * time.Minute
	// bridgeHoldWindow is how long each keepalive hold runs for. The platform
	// caps a hold at an hour, so a long session is a series of renewals.
	bridgeHoldWindow = time.Hour
	// bridgeRenewEvery is how often an active bridge renews the hold. It has
	// to be comfortably inside bridgeHoldWindow so a slow renewal never
	// leaves a gap the sprite could pause in.
	bridgeRenewEvery = 30 * time.Minute
)

var (
	bridgeListen     string
	bridgeRemotePort int
	bridgeOrg        string
)

// bridgeCmd forwards a local port to a port on a sprite, waking the sprite when
// a client connects and holding it Active while one is attached.
var bridgeCmd = &cobra.Command{
	Use:   "bridge <sprite>",
	Short: "Forward a local port to a sprite, waking it when a client connects",
	Long: `bridge listens on a local port and forwards each connection to a port on a
sprite, waking the sprite on the first connection and holding it Active for as
long as a client is attached.

Run it on a machine that stays up. It lets a client that has no idea what a
sprite is — a phone app that only knows a host and a port — treat one as an
always-on server.

  sp bridge my-sprite --listen 0.0.0.0:4001 --port 3773

The first connection after the sprite has paused takes as long as a cold start.
Everything after that is a normal forward.`,
	Args: cobra.ExactArgs(1),
	RunE: runBridge,
}

func init() {
	bridgeCmd.Flags().StringVar(&bridgeListen, "listen", "127.0.0.1:3773", "local address to listen on")
	bridgeCmd.Flags().IntVar(&bridgeRemotePort, "port", 3773, "port to forward to on the sprite")
	bridgeCmd.Flags().StringVar(&bridgeOrg, "org", "", "Sprites organization (default: the CLI default)")
	rootCmd.AddCommand(bridgeCmd)
}

// runBridge resolves the sprite, starts the listener and serves connections
// until interrupted.
func runBridge(cmd *cobra.Command, args []string) error {
	spriteName := args[0]
	org := bridgeOrg
	client := sprite.NewClient(org)

	if exists, err := client.Exists(spriteName); err != nil {
		return fmt.Errorf("checking sprite: %w", err)
	} else if !exists {
		return fmt.Errorf("sprite %q does not exist", spriteName)
	}

	ln, err := net.Listen("tcp", bridgeListen)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", bridgeListen, err)
	}
	defer ln.Close()

	b := &bridge{
		client: client,
		org:    org,
		sprite: spriteName,
		port:   bridgeRemotePort,
	}

	fmt.Printf("Bridging %s -> %s:%d\n", ln.Addr(), spriteName, bridgeRemotePort)
	fmt.Println("The sprite wakes on the first connection and is held Active while one is open.")

	for {
		conn, err := ln.Accept()
		if err != nil {
			return fmt.Errorf("accepting: %w", err)
		}
		go b.serve(conn)
	}
}

// bridge holds the state shared by every connection: which sprite to reach and
// whether a keepalive hold is currently running for it.
type bridge struct {
	client *sprite.Client
	org    string
	sprite string
	port   int

	mu      sync.Mutex
	active  int
	release *time.Timer
	renew   *time.Ticker
	stop    chan struct{}
}

// serve handles one client connection: wake, hold, pipe, then release.
func (b *bridge) serve(conn net.Conn) {
	defer conn.Close()

	b.acquire()
	defer b.releaseSoon()

	// Waking is explicit rather than left to the proxy call, because a cold
	// start takes long enough that the proxy may give up first. Any exec wakes
	// the sprite and returns once it is running.
	if _, err := b.client.Exec(sprite.ExecOptions{
		Sprite:  b.sprite,
		Org:     b.org,
		Command: []string{"true"},
	}); err != nil {
		fmt.Fprintf(os.Stderr, "bridge: waking %s: %v\n", b.sprite, err)
		return
	}

	args := b.client.BuildExecArgs(sprite.ExecOptions{Sprite: b.sprite, Org: b.org})
	// BuildExecArgs builds an exec; this is a proxy, so replace the verb and
	// drop the trailing command separator it adds for exec.
	proxyArgs := append([]string{"proxy", "--stdio", fmt.Sprintf("127.0.0.1:%d", b.port)}, execFlagsOnly(args)...)

	cmd := exec.Command("sprite", proxyArgs...)
	cmd.Stderr = os.Stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		fmt.Fprintf(os.Stderr, "bridge: stdin pipe: %v\n", err)
		return
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		fmt.Fprintf(os.Stderr, "bridge: stdout pipe: %v\n", err)
		return
	}
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "bridge: starting proxy: %v\n", err)
		return
	}

	// Either direction closing ends the connection: a client hanging up and
	// the sprite side going away are both just "this session is over".
	done := make(chan struct{}, 2)
	go func() { io.Copy(stdin, conn); stdin.Close(); done <- struct{}{} }()
	go func() { io.Copy(conn, stdout); done <- struct{}{} }()
	<-done

	cmd.Process.Kill()
	cmd.Wait()
}

// execFlagsOnly strips the leading verb and the trailing "--" separator from a
// BuildExecArgs result, leaving just the org and sprite flags that every sprite
// subcommand takes.
func execFlagsOnly(args []string) []string {
	out := []string{}
	for _, a := range args[1:] {
		if a == "--" {
			break
		}
		out = append(out, a)
	}
	return out
}

// acquire registers a connection, starting the hold if this is the first one
// and cancelling any pending release.
func (b *bridge) acquire() {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.release != nil {
		b.release.Stop()
		b.release = nil
	}
	b.active++
	if b.active > 1 {
		return
	}

	b.startHold()
	b.stop = make(chan struct{})
	b.renew = time.NewTicker(bridgeRenewEvery)
	go b.renewLoop(b.renew, b.stop)
}

// releaseSoon deregisters a connection and, when it was the last one, schedules
// the hold to be dropped after the idle grace period.
func (b *bridge) releaseSoon() {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.active--
	if b.active > 0 {
		return
	}
	b.release = time.AfterFunc(bridgeIdleGrace, func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		if b.active > 0 {
			return
		}
		if b.renew != nil {
			b.renew.Stop()
			close(b.stop)
			b.renew = nil
		}
		keepalive.Stop(b.client, b.sprite, b.org)
		fmt.Printf("bridge: released hold on %s\n", b.sprite)
	})
}

// renewLoop re-arms the hold while connections are open, because a hold is
// bounded and a session can outlast one window.
func (b *bridge) renewLoop(t *time.Ticker, stop chan struct{}) {
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			b.mu.Lock()
			if b.active > 0 {
				b.startHold()
			}
			b.mu.Unlock()
		}
	}
}

// startHold asks the sprite to stay Active. A failure is reported but not
// fatal: the forward still works, the sprite is just free to pause.
func (b *bridge) startHold() {
	if err := keepalive.Start(keepalive.Options{
		Sprite: b.sprite,
		Org:    b.org,
		For:    bridgeHoldWindow,
	}); err != nil {
		fmt.Fprintf(os.Stderr, "bridge: holding %s awake: %v\n", b.sprite, err)
	}
}
