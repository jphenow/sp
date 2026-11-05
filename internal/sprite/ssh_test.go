package sprite

import (
	"slices"
	"strings"
	"testing"
)

func TestSSHUser(t *testing.T) {
	t.Run("builds the gateway's three-field username", func(t *testing.T) {
		got, err := SSHUser("gh-superfly-sprite-env-blue", "jon-phenow")
		if err != nil {
			t.Fatal(err)
		}
		if want := "gh-superfly-sprite-env-blue--default--jon-phenow"; got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	})

	// The gateway splits on "--" and requires exactly three fields, so these
	// cannot be addressed at all. Refusing here keeps them on the exec
	// transport instead of failing as a closed connection with no reason.
	t.Run("refuses a name that would make the username ambiguous", func(t *testing.T) {
		if _, err := SSHUser("gh-jphenow--gameservers--red", "jon-phenow"); err == nil {
			t.Fatal("expected a sprite name containing -- to be refused")
		}
		if _, err := SSHUser("blue", "my--org"); err == nil {
			t.Fatal("expected an org containing -- to be refused")
		}
	})

	t.Run("refuses a missing org, which the username has no default for", func(t *testing.T) {
		if _, err := SSHUser("blue", ""); err == nil {
			t.Fatal("expected a missing org to be refused")
		}
	})
}

func TestSSHInteractiveArgs(t *testing.T) {
	args := SSHInteractiveArgs("blue--default--org", "/tmp/.sp-connect-x.sh", "/tmp/ssh-blue.log")
	if !slices.Contains(args, "-t") {
		t.Errorf("a TTY is required for an interactive session: %v", args)
	}
	if !slices.Contains(args, "ServerAliveInterval=15") {
		t.Errorf("the keepalive is the reason for this transport: %v", args)
	}
	if got := args[len(args)-1]; got != "/tmp/.sp-connect-x.sh" {
		t.Errorf("the script should be the last argument, got %q", got)
	}
	if !strings.HasSuffix(args[slices.Index(args, "--")-1], "@"+SSHHost) {
		t.Errorf("the destination should precede the separator: %v", args)
	}
	// The log is the only account of a drop on this transport, so it has to be
	// asked for explicitly and sent to a file rather than to the terminal.
	if !slices.Contains(args, "-v") || !slices.Contains(args, "/tmp/ssh-blue.log") {
		t.Errorf("the connection log should be requested: %v", args)
	}

	// Without a path, nothing is logged and nothing is made verbose: a verbose
	// client with no log file writes its debugging into the session.
	plain := SSHInteractiveArgs("blue--default--org", "/tmp/.sp-connect-x.sh", "")
	if slices.Contains(plain, "-v") || slices.Contains(plain, "-E") {
		t.Errorf("verbose logging should be opt-in: %v", plain)
	}
}

func TestSSHRemoteScriptPath(t *testing.T) {
	// One script per session, and nothing in the name the remote shell would
	// have to quote.
	if got := SSHRemoteScriptPath("sp-my/repo name"); got != "/tmp/.sp-connect-sp-my-repo-name.sh" {
		t.Fatalf("got %q", got)
	}
	if got := SSHRemoteScriptPath(""); got != "/tmp/.sp-connect-session.sh" {
		t.Fatalf("got %q", got)
	}
}
