package sync

import (
	"testing"
)

func TestComputeSSHPort(t *testing.T) {
	tests := []struct {
		name string
	}{
		{"gh-superfly--flyctl"},
		{"gh-jphenow--gameservers"},
		{"local-my-project"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			port := ComputeSSHPort(tt.name)
			if port < 10000 || port > 60000 {
				t.Errorf("port %d out of range [10000, 60000]", port)
			}
		})
	}

	// Deterministic: same name always gives same port
	p1 := ComputeSSHPort("test-sprite")
	p2 := ComputeSSHPort("test-sprite")
	if p1 != p2 {
		t.Errorf("non-deterministic: %d != %d", p1, p2)
	}

	// Different names should (usually) give different ports
	p3 := ComputeSSHPort("different-sprite")
	if p1 == p3 {
		t.Log("Warning: hash collision (unlikely but possible)")
	}
}

func TestSessionName(t *testing.T) {
	got := SessionName("gh-superfly--flyctl")
	want := "sprite-gh-superfly--flyctl"
	if got != want {
		t.Errorf("SessionName() = %q, want %q", got, want)
	}
}

func TestSSHHostAlias(t *testing.T) {
	got := SSHHostAlias("gh-superfly--flyctl")
	want := "sprite-mutagen-gh-superfly--flyctl"
	if got != want {
		t.Errorf("SSHHostAlias() = %q, want %q", got, want)
	}
}

func TestParseMutagenStatus(t *testing.T) {
	output := `--------------------------------------------------------------------------------
Name: sprite-gh-superfly--flyctl
Identifier: sync_abc123def456
Alpha:
	URL: /Users/jon/workspace/superfly/flyctl
	Connected: Yes
Beta:
	URL: sprite-mutagen-gh-superfly--flyctl:/home/sprite/flyctl
	Connected: Yes
Status: Watching for changes
--------------------------------------------------------------------------------`

	state := parseMutagenStatus("sprite-gh-superfly--flyctl", output)

	if state.MutagenID != "sync_abc123def456" {
		t.Errorf("MutagenID = %q, want %q", state.MutagenID, "sync_abc123def456")
	}
	if state.Status != "watching" {
		t.Errorf("Status = %q, want %q", state.Status, "watching")
	}
	if !state.AlphaConnected {
		t.Error("Alpha should be connected")
	}
	if !state.BetaConnected {
		t.Error("Beta should be connected")
	}
	if state.Conflicts != 0 {
		t.Errorf("Conflicts = %d, want 0", state.Conflicts)
	}
}

func TestParseMutagenStatusConnecting(t *testing.T) {
	output := `--------------------------------------------------------------------------------
Name: sprite-gh-jphenow--market-moves
Identifier: sync_qUfwEyNk6L4dH0zhnn0EJbHFGNXpfu7xrFzxMiF1wav
Alpha:
	URL: /Users/jon/Code/market-moves
	Connected: Yes
Beta:
	URL: sprite-mutagen-gh-jphenow--market-moves:/home/sprite/market-moves
	Connected: No
Status: Connecting to beta
--------------------------------------------------------------------------------`

	state := parseMutagenStatus("sprite-gh-jphenow--market-moves", output)

	if state.Status != "connecting" {
		t.Errorf("Status = %q, want %q", state.Status, "connecting")
	}
	if !state.AlphaConnected {
		t.Error("Alpha should be connected")
	}
	if state.BetaConnected {
		t.Error("Beta should NOT be connected")
	}
}

func TestParseMutagenStatusWithError(t *testing.T) {
	output := `--------------------------------------------------------------------------------
Name: sprite-gh-superfly--mpg-ui
Identifier: sync_ErLuVjov2h8BTeGnDCGd0rusNjnxphXevwqK8WwL9Zf
Alpha:
	URL: /Users/jon/workspace/superfly/mpg-ui
	Connected: Yes
Beta:
	URL: sprite-mutagen-gh-superfly--mpg-ui:/home/sprite/mpg-ui
	Connected: No
Last error: beta polling error: unable to receive poll response: unable to read message length: unexpected EOF
Status: Connecting to beta
--------------------------------------------------------------------------------`

	state := parseMutagenStatus("sprite-gh-superfly--mpg-ui", output)

	if state.Status != "connecting" {
		t.Errorf("Status = %q, want %q", state.Status, "connecting")
	}
	if state.LastError == "" {
		t.Error("LastError should be populated")
	}
	if state.LastError != "beta polling error: unable to receive poll response: unable to read message length: unexpected EOF" {
		t.Errorf("LastError = %q", state.LastError)
	}
}

func TestNormalizeMutagenStatus(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"Watching for changes", "watching"},
		{"Scanning files", "syncing"},
		{"Staging files on alpha", "syncing"},
		{"Transitioning", "syncing"},
		{"Saving archive", "syncing"},
		{"Connecting to beta", "connecting"},
		{"Halted on root emptied", "error"},
		{"Something unknown", "unknown"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := normalizeMutagenStatus(tt.input)
			if got != tt.want {
				t.Errorf("normalizeMutagenStatus(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}
