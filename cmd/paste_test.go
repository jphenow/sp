package cmd

import "testing"

// Command lines taken from a real machine running sp sessions.
func TestSpriteFlagsFrom(t *testing.T) {
	cases := []struct {
		name      string
		args      string
		wantName  string
		wantOrg   string
		wantFound bool
	}{
		{
			name:      "attach",
			args:      "/Users/jon/bin/sprite attach 10528 -s gh-jphenow--gameservers--green",
			wantName:  "gh-jphenow--gameservers--green",
			wantFound: true,
		},
		{
			name:      "exec with org and tty",
			args:      "sprite exec -o jon-phenow -s gh-superfly--sprites-api--blue --tty --dir /home/sprite/x -- sh -c stty rows 50",
			wantName:  "gh-superfly--sprites-api--blue",
			wantOrg:   "jon-phenow",
			wantFound: true,
		},
		{
			// The remote command after `--` can carry its own -s; it must not win.
			name:      "remote command has its own -s",
			args:      "sprite exec -s gh-jphenow--sp -- sh -c tmux new-session -A -s bash",
			wantName:  "gh-jphenow--sp",
			wantFound: true,
		},
		{name: "not the sprite client", args: "/Users/jon/workspace/superfly/sprite-repo/sp-bin jphenow/gameservers blue"},
		{name: "shell", args: "-zsh"},
		{name: "sprite with no -s", args: "sprite list"},
		{name: "empty", args: ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			name, org, ok := spriteFlagsFrom(c.args)
			if ok != c.wantFound || name != c.wantName || org != c.wantOrg {
				t.Errorf("= (%q, %q, %v), want (%q, %q, %v)", name, org, ok, c.wantName, c.wantOrg, c.wantFound)
			}
		})
	}
}

// The sprite client is a grandchild of the pane: pane shell -> sp -> sprite.
func TestSpriteUnderPID(t *testing.T) {
	procs := []procEntry{
		{pid: 100, ppid: 1, args: "-zsh"},
		{pid: 200, ppid: 100, args: "/path/sp-bin jphenow/gameservers green"},
		{pid: 300, ppid: 200, args: "/Users/jon/bin/sprite attach 10528 -s gh-jphenow--gameservers--green"},
		{pid: 400, ppid: 1, args: "sprite exec -s some-other-sprite -- sleep 1"},
	}
	name, _ := spriteUnderPID(procs, 100)
	if name != "gh-jphenow--gameservers--green" {
		t.Errorf("name = %q, want the sprite under this pane", name)
	}
	if name, _ := spriteUnderPID(procs, 999); name != "" {
		t.Errorf("unknown pane should find nothing, got %q", name)
	}
	// An unrelated sprite process elsewhere in the tree must not be picked up.
	if name, _ := spriteUnderPID(procs, 200); name != "gh-jphenow--gameservers--green" {
		t.Errorf("name = %q, want the descendant of the given root", name)
	}
}
