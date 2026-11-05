package sprite

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// Sprites can also be reached over SSH, which is a better transport for an
// interactive session than the CLI's exec: it gets the initial window size
// right, forwards SIGWINCH for the life of the session, has its own keepalive
// watchdog, carries port forwards on the same connection, and wakes a cold
// sprite the same way. It is opt-in for now — see `sp connect --ssh`.

const (
	// SSHHost is the gateway. It routes by username, not by hostname.
	SSHHost = "sprites.dev"
	// sshCluster is the cluster segment of the username. Sprites created by
	// sp all live in the default cluster.
	sshCluster = "default"
)

// sshRemoteScriptName keeps the generated script's filename predictable and
// free of anything the shell would have to quote.
var sshRemoteScriptName = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// SSHUser builds the gateway username for a sprite.
//
// The gateway splits the username on "--" and requires exactly three fields,
// so a sprite or org whose own name contains the separator cannot be addressed
// this way at all. Those names predate the separator being reserved; they stay
// on the exec transport rather than failing somewhere less obvious.
func SSHUser(spriteName, org string) (string, error) {
	switch {
	case spriteName == "":
		return "", fmt.Errorf("no sprite name")
	case org == "":
		return "", fmt.Errorf("ssh needs the organization slug; pass -o or set it in the sp config")
	case strings.Contains(spriteName, "--"):
		return "", fmt.Errorf("sprite %q cannot be reached over ssh: the gateway splits the username on %q", spriteName, "--")
	case strings.Contains(org, "--"):
		return "", fmt.Errorf("organization %q cannot be reached over ssh: the gateway splits the username on %q", org, "--")
	}

	return spriteName + "--" + sshCluster + "--" + org, nil
}

// sshBaseArgs are the options every sp ssh invocation shares.
//
// ServerAliveInterval is the whole point: ssh notices a connection that has
// stopped answering on its own, which is the watchdog the exec transport does
// not have. BatchMode keeps a broken connection from stopping on a prompt.
//
// ConnectTimeout bounds a dial made while the network is away, which the
// default leaves to the operating system's TCP timeout — well over a minute on
// macOS. sp reconnects in a loop and treats an attempt that lasted long enough
// as a connection that worked, so a dial that hangs for minutes is not just
// slow, it is indistinguishable from a session. Still generous next to the
// handshake itself: the gateway answers immediately, and waking the sprite
// happens after the session is requested.
func sshBaseArgs() []string {
	return []string{
		"-o", "ServerAliveInterval=15",
		"-o", "ServerAliveCountMax=3",
		"-o", "ConnectTimeout=30",
		"-o", "BatchMode=yes",
	}
}

// SSHRemoteScriptPath names where a session's generated script lives on the
// sprite. One per tmux session, so parallel variants do not collide.
func SSHRemoteScriptPath(tmuxSession string) string {
	name := sshRemoteScriptName.ReplaceAllString(tmuxSession, "-")
	if name == "" {
		name = "session"
	}

	return "/tmp/.sp-connect-" + name + ".sh"
}

// SSHInteractiveArgs builds the argv for the interactive session: a real TTY
// running a script that is already on the sprite.
//
// logPath, when given, is where ssh keeps its own account of the connection.
// sp sees a drop only as an exit status and whatever reached the terminal,
// which for ssh is usually nothing at all — the client is quiet by design, so
// a session that ends has no stated cause. One -v is enough to say which end
// gave up and why (a keepalive that went unanswered reads differently from a
// channel the server closed), and it goes to a file rather than to the
// terminal, where it would be mixed into the session's own output.
func SSHInteractiveArgs(user, remoteScript, logPath string) []string {
	args := sshBaseArgs()
	if logPath != "" {
		args = append(args, "-v", "-E", logPath)
	}

	return append(args, "-t", user+"@"+SSHHost, "--", "bash", remoteScript)
}

// SSHSessionLogPath names the file ssh writes its connection log to, one per
// sprite.
//
// ssh appends, so the caller truncates it when a session starts: one file then
// holds the whole of the current run, every reconnect attempt included, which
// is the unit anyone investigating a drop actually wants.
func SSHSessionLogPath(spriteName string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	name := sshRemoteScriptName.ReplaceAllString(spriteName, "-")
	if name == "" {
		name = "session"
	}

	return filepath.Join(home, ".config", "sp", "ssh-"+name+".log"), nil
}

// WriteRemoteScript puts the session script on the sprite, passing it over
// stdin.
//
// The script carries live tokens, because the tmux setenv lines embed their
// values. Sending it on stdin keeps it out of every argv — the local ssh
// process's and the remote shell's alike — which is strictly better than the
// exec transport, where the same script is an argument to `sh -c`.
func WriteRemoteScript(user, remoteScript, script string) error {
	args := append(sshBaseArgs(), user+"@"+SSHHost, "--",
		"sh", "-c", "umask 077; cat > "+remoteScript)
	cmd := exec.Command("ssh", args...)
	cmd.Stdin = strings.NewReader(script)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("writing the session script over ssh: %w\n%s", err, strings.TrimSpace(string(out)))
	}

	return nil
}
