package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/jphenow/sp/internal/setup"
	"github.com/jphenow/sp/internal/sprite"
)

// Claude Code reads an image when its path appears in the prompt — verified
// with a bare path and no other wording. But a sprite can't see your Mac's
// clipboard, and a path dragged from Finder points at a file the sprite doesn't
// have, so pasting a screenshot into a sprite session does nothing today.
//
// `sp paste` closes that gap from the local side: it takes the image off the
// clipboard, uploads it to the sprite, and types the remote path into the
// prompt. Bind it to a key in tmux and pasting a screenshot works the way it
// does locally.
//
// Two design points worth keeping:
//
//   - Keystrokes go to the LOCAL tmux pane, not the sprite's tmux. They flow
//     over the attach sp already holds, arriving exactly as if typed. That
//     costs one sprite call (the upload) instead of two.
//   - Text on the clipboard still pastes normally. If this is bound over Cmd-V,
//     anything else would break ordinary pasting.

var (
	pastePaneID string
	pasteSprite string
)

// pasteCmd uploads a clipboard image to the sprite and types its path.
var pasteCmd = &cobra.Command{
	Use:   "paste",
	Short: "Paste the clipboard into a sprite session, uploading images to the sprite",
	Long: `Paste the system clipboard into a running sp session.

An image is uploaded to the sprite and its remote path typed into the prompt,
so Claude can read it. Anything else pastes as ordinary text.

Meant to be bound to a key in your local tmux, which is what makes Cmd-V work:

  # ~/.config/ghostty/config
  keybind = cmd+shift+v=text:\x1b\x16

  # ~/.tmux.conf
  bind -n M-C-v run-shell -b "sp paste -t #{pane_id}"`,
	RunE: runPaste,
}

func init() {
	pasteCmd.Flags().StringVarP(&pastePaneID, "pane", "t", "", "local tmux pane to paste into (default: the active pane)")
	pasteCmd.Flags().StringVar(&pasteSprite, "sprite", "", "sprite to upload to (default: detected from the pane's sp session)")
	rootCmd.AddCommand(pasteCmd)
}

// spriteImageDir is where uploaded clipboard images land on the sprite.
const spriteImageDir = "/home/sprite/.sp-images"

// pasteImageRetention is how long uploaded images are kept on the sprite. They
// are pruned by the same call that uploads, so this costs nothing extra.
const pasteImageRetentionDays = 7

func runPaste(cmd *cobra.Command, args []string) error {
	if runtime.GOOS != "darwin" {
		return fmt.Errorf("sp paste reads the macOS clipboard; not supported on %s", runtime.GOOS)
	}
	pane := resolvePane()

	tmpDir, err := os.MkdirTemp("", "sp-paste-*")
	if err != nil {
		return fmt.Errorf("temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	img, err := clipboardImage(tmpDir)
	if err != nil {
		// No image on the clipboard is the common case: paste text instead, so
		// this can sit on the normal paste key without breaking it.
		return pasteText(pane)
	}

	spriteName, org, err := pasteTarget(pane)
	if err != nil {
		notifyPane(pane, "sp paste: "+err.Error())
		return err
	}

	remote := fmt.Sprintf("%s/%s.png", spriteImageDir, time.Now().Format("20060102-150405"))
	client := sprite.NewClient(org)
	// Prune old images in the same call that uploads the new one; a call is
	// almost entirely connection setup, so the cleanup rides along for free.
	prune := fmt.Sprintf("find %s -type f -mtime +%d -delete 2>/dev/null || true",
		spriteImageDir, pasteImageRetentionDays)
	if err := setup.UploadFilesRunning(client, spriteName, map[string]string{img: remote}, prune); err != nil {
		notifyPane(pane, "sp paste: upload failed, see sp log")
		return fmt.Errorf("uploading image to %s: %w", spriteName, err)
	}

	// Trailing space so the next thing typed doesn't run into the path.
	return sendKeys(pane, remote+" ")
}

// resolvePane returns the tmux pane to paste into: the -t flag, else the pane
// tmux exported to this process, else empty, which tmux reads as the active one.
func resolvePane() string {
	if pastePaneID != "" {
		return pastePaneID
	}
	return os.Getenv("TMUX_PANE")
}

// clipboardImage writes the clipboard's image to a PNG in dir and returns its
// path, or an error when the clipboard holds no image.
//
// AppleScript is the only way at this; pbpaste is text-only and pngpaste isn't
// installed by default. Screenshots arrive as PNG, while apps like Preview put
// TIFF on the clipboard, so both are tried and TIFF is converted with sips.
func clipboardImage(dir string) (string, error) {
	png := filepath.Join(dir, "clipboard.png")
	if err := writeClipboardClass("PNGf", png); err == nil && fileHasContent(png) {
		return png, nil
	}

	tiff := filepath.Join(dir, "clipboard.tiff")
	if err := writeClipboardClass("TIFF", tiff); err != nil || !fileHasContent(tiff) {
		return "", fmt.Errorf("no image on the clipboard")
	}
	if out, err := exec.Command("sips", "-s", "format", "png", tiff, "--out", png).CombinedOutput(); err != nil {
		return "", fmt.Errorf("converting clipboard TIFF to PNG: %w: %s", err, out)
	}
	if !fileHasContent(png) {
		return "", fmt.Errorf("no image on the clipboard")
	}
	return png, nil
}

// writeClipboardClass asks AppleScript for the clipboard as one pasteboard
// class and writes it to path. It fails when the clipboard holds no such class.
func writeClipboardClass(class, path string) error {
	script := fmt.Sprintf(
		`set f to open for access POSIX file %q with write permission
set eof f to 0
write (the clipboard as «class %s») to f
close access f`, path, class)
	cmd := exec.Command("osascript", "-e", script)
	if out, err := cmd.CombinedOutput(); err != nil {
		// Best-effort: AppleScript leaves the file open if it failed mid-way.
		_ = exec.Command("osascript", "-e", fmt.Sprintf(`try
close access POSIX file %q
end try`, path)).Run()
		return fmt.Errorf("clipboard as %s: %w: %s", class, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// fileHasContent reports whether path exists and is non-empty. AppleScript
// creates the file before it knows whether the conversion works, so an empty
// file means "no image of that class".
func fileHasContent(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Size() > 0
}

// pasteText pastes the clipboard as text into the pane, preserving bracketed
// paste so multi-line text lands as a paste rather than as typed input.
func pasteText(pane string) error {
	text, err := exec.Command("pbpaste").Output()
	if err != nil {
		return fmt.Errorf("reading clipboard: %w", err)
	}
	if len(text) == 0 {
		return nil
	}
	load := exec.Command("tmux", "load-buffer", "-b", "sp-paste", "-")
	load.Stdin = strings.NewReader(string(text))
	if out, err := load.CombinedOutput(); err != nil {
		return fmt.Errorf("tmux load-buffer: %w: %s", err, out)
	}
	args := []string{"paste-buffer", "-p", "-d", "-b", "sp-paste"}
	if pane != "" {
		args = append(args, "-t", pane)
	}
	if out, err := exec.Command("tmux", args...).CombinedOutput(); err != nil {
		return fmt.Errorf("tmux paste-buffer: %w: %s", err, out)
	}
	return nil
}

// sendKeys types literal text into the pane, which forwards it over the
// existing attach into whatever is running on the sprite.
func sendKeys(pane, text string) error {
	args := []string{"send-keys"}
	if pane != "" {
		args = append(args, "-t", pane)
	}
	args = append(args, "-l", text)
	if out, err := exec.Command("tmux", args...).CombinedOutput(); err != nil {
		return fmt.Errorf("tmux send-keys: %w: %s", err, out)
	}
	return nil
}

// notifyPane shows a short message in tmux's status line. sp paste normally
// runs detached from a key binding, where stderr goes nowhere a user can see.
func notifyPane(pane, msg string) {
	args := []string{"display-message"}
	if pane != "" {
		args = append(args, "-t", pane)
	}
	args = append(args, msg)
	_ = exec.Command("tmux", args...).Run()
}

// pasteTarget returns the sprite (and org) the pane is attached to: the
// --sprite flag if given, otherwise read from the sprite client running under
// that pane.
func pasteTarget(pane string) (string, string, error) {
	if pasteSprite != "" {
		return pasteSprite, "", nil
	}
	pid, err := panePID(pane)
	if err != nil {
		return "", "", err
	}
	name, org := spriteUnderPID(processTable(), pid)
	if name == "" {
		return "", "", fmt.Errorf("no sprite session found in this pane; pass --sprite")
	}
	return name, org, nil
}

// panePID returns the pane's own process id, the root of the tree sp and the
// sprite client run under.
func panePID(pane string) (int, error) {
	args := []string{"display-message", "-p"}
	if pane != "" {
		args = append(args, "-t", pane)
	}
	args = append(args, "#{pane_pid}")
	out, err := exec.Command("tmux", args...).Output()
	if err != nil {
		return 0, fmt.Errorf("asking tmux for the pane pid (is this a tmux pane?): %w", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		return 0, fmt.Errorf("unexpected pane pid %q", strings.TrimSpace(string(out)))
	}
	return pid, nil
}

// procEntry is one row of the process table.
type procEntry struct {
	pid  int
	ppid int
	args string
}

// processTable snapshots every process, for walking a pane's descendants.
func processTable() []procEntry {
	out, err := exec.Command("ps", "-eo", "pid=,ppid=,args=").Output()
	if err != nil {
		return nil
	}
	var entries []procEntry
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		pid, err1 := strconv.Atoi(fields[0])
		ppid, err2 := strconv.Atoi(fields[1])
		if err1 != nil || err2 != nil {
			continue
		}
		entries = append(entries, procEntry{pid: pid, ppid: ppid, args: strings.Join(fields[2:], " ")})
	}
	return entries
}

// spriteUnderPID finds the sprite name and org of the `sprite` client running
// somewhere beneath root, by reading its -s/-o flags.
//
// The pane runs a shell, which runs sp, which runs `sprite exec|attach -s
// <name>`, so the answer is always a descendant rather than the pane process
// itself.
func spriteUnderPID(procs []procEntry, root int) (string, string) {
	children := map[int][]procEntry{}
	for _, p := range procs {
		children[p.ppid] = append(children[p.ppid], p)
	}
	queue := []int{root}
	for len(queue) > 0 {
		pid := queue[0]
		queue = queue[1:]
		for _, child := range children[pid] {
			if name, org, ok := spriteFlagsFrom(child.args); ok {
				return name, org
			}
			queue = append(queue, child.pid)
		}
	}
	return "", ""
}

// spriteFlagsFrom pulls -s <name> and -o <org> out of a sprite client command
// line, ignoring anything after the `--` that separates the remote command
// (which can contain a -s of its own).
func spriteFlagsFrom(args string) (string, string, bool) {
	fields := strings.Fields(args)
	if len(fields) == 0 || filepath.Base(fields[0]) != "sprite" {
		return "", "", false
	}
	var name, org string
scan:
	for i := 1; i < len(fields); i++ {
		switch fields[i] {
		case "--":
			break scan
		case "-s", "--sprite":
			if i+1 < len(fields) {
				name = fields[i+1]
				i++
			}
		case "-o", "--org":
			if i+1 < len(fields) {
				org = fields[i+1]
				i++
			}
		}
	}
	if name == "" {
		return "", "", false
	}
	return name, org, true
}
