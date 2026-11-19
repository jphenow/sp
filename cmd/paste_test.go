package cmd

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

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

func TestPasteRemoteName(t *testing.T) {
	cases := map[string]string{
		"report.pdf":       "report.pdf",
		"My Report v2.pdf": "My-Report-v2.pdf",
		"notes:draft.md":   "notes-draft.md",
		"a'b\"c.txt":       "a-b-c.txt",
		"...":              "file",
		"..":               "file",
		".env":             ".env",
		"Screenshot 1.png": "Screenshot-1.png",
	}
	for in, want := range cases {
		if got := pasteRemoteName(in); got != want {
			t.Errorf("pasteRemoteName(%q) = %q, want %q", in, got, want)
		}
	}
}

// writePasteboard puts values on a private pasteboard so a test never touches
// the clipboard the person is using.
func writePasteboard(t *testing.T, name string, script string) {
	t.Helper()
	full := `ObjC.import("AppKit");
(function () {
  var pb = $.NSPasteboard.pasteboardWithName("` + name + `");
  pb.clearContents;
  ` + script + `
  return "ok";
})()`
	if out, err := exec.Command("osascript", "-l", "JavaScript", "-e", full).CombinedOutput(); err != nil {
		t.Fatalf("seeding pasteboard: %v: %s", err, out)
	}
}

// TestClipboardFiles reads real file references off a real pasteboard, because
// the behaviour worth being sure of is AppKit's, not sp's: that several files
// come back from one copy, and that a pasteboard holding text yields no files
// at all — including text that is itself a file:// URL, which is what keeps
// pasting the text of a path from uploading anything.
func TestClipboardFiles(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("reads the macOS pasteboard")
	}
	dir := t.TempDir()
	one := filepath.Join(dir, "one.txt")
	two := filepath.Join(dir, "two with spaces.txt")
	for _, p := range []string{one, two} {
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	board := "sp-paste-test"
	clipboardPasteboard = board
	defer func() { clipboardPasteboard = "" }()

	writePasteboard(t, board, `var urls = $.NSMutableArray.alloc.init;
  urls.addObject($.NSURL.fileURLWithPath($("`+one+`")));
  urls.addObject($.NSURL.fileURLWithPath($("`+two+`")));
  pb.writeObjects(urls);`)

	got, err := clipboardFiles()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != one || got[1] != two {
		t.Fatalf("got %q, want %q", got, []string{one, two})
	}

	writePasteboard(t, board, `pb.setStringForType($("file://`+one+`"), $.NSPasteboardTypeString);`)
	got, err = clipboardFiles()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("got %q from a text pasteboard, want none", got)
	}
}
