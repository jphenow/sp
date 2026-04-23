package setup

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseSetupConf(t *testing.T) {
	dir := t.TempDir()
	confPath := filepath.Join(dir, "setup.conf")

	content := `# This is a comment
[files]
~/.tmux.conf
~/.config/opencode/config.toml [always]
~/.local/share/opencode/auth.json [newest]
~/.config/local -> ~/.config/remote
~/.config/foo -> ~/.config/bar [always]

[commands]
! command -v opencode :: curl -fsSL https://opencode.ai/install | bash &
command -v git :: echo "git found"
`
	if err := os.WriteFile(confPath, []byte(content), 0o644); err != nil {
		t.Fatalf("writing test config: %v", err)
	}

	conf, err := ParseSetupConf(confPath)
	if err != nil {
		t.Fatalf("parsing: %v", err)
	}
	if conf == nil {
		t.Fatal("expected non-nil conf")
	}

	// Check files
	if len(conf.Files) != 5 {
		t.Fatalf("expected 5 file entries, got %d", len(conf.Files))
	}

	// ~/.tmux.conf - default newest mode
	if conf.Files[0].Mode != "newest" {
		t.Errorf("file[0] mode = %q, want 'newest'", conf.Files[0].Mode)
	}
	if !filepath.IsAbs(conf.Files[0].Source) {
		t.Errorf("file[0] source should be absolute, got %q", conf.Files[0].Source)
	}

	// ~/.config/opencode/config.toml [always]
	if conf.Files[1].Mode != "always" {
		t.Errorf("file[1] mode = %q, want 'always'", conf.Files[1].Mode)
	}

	// ~/.local/share/opencode/auth.json [newest]
	if conf.Files[2].Mode != "newest" {
		t.Errorf("file[2] mode = %q, want 'newest'", conf.Files[2].Mode)
	}

	// ~/.config/local -> ~/.config/remote
	if conf.Files[3].Dest != "/home/sprite/.config/remote" {
		t.Errorf("file[3] dest = %q, want '/home/sprite/.config/remote'", conf.Files[3].Dest)
	}
	if conf.Files[3].Mode != "newest" {
		t.Errorf("file[3] mode = %q, want 'newest'", conf.Files[3].Mode)
	}

	// ~/.config/foo -> ~/.config/bar [always]
	if conf.Files[4].Mode != "always" {
		t.Errorf("file[4] mode = %q, want 'always'", conf.Files[4].Mode)
	}
	if conf.Files[4].Dest != "/home/sprite/.config/bar" {
		t.Errorf("file[4] dest = %q, want '/home/sprite/.config/bar'", conf.Files[4].Dest)
	}

	// Check commands
	if len(conf.Commands) != 2 {
		t.Fatalf("expected 2 command entries, got %d", len(conf.Commands))
	}

	if conf.Commands[0].Condition != "! command -v opencode" {
		t.Errorf("cmd[0] condition = %q", conf.Commands[0].Condition)
	}
	if conf.Commands[0].Command != "curl -fsSL https://opencode.ai/install | bash &" {
		t.Errorf("cmd[0] command = %q", conf.Commands[0].Command)
	}

	if conf.Commands[1].Condition != "command -v git" {
		t.Errorf("cmd[1] condition = %q", conf.Commands[1].Condition)
	}
}

func TestParseSetupConfNotFound(t *testing.T) {
	conf, err := ParseSetupConf("/nonexistent/path")
	if err != nil {
		t.Fatalf("unexpected error for missing file: %v", err)
	}
	if conf != nil {
		t.Error("expected nil conf for missing file")
	}
}

func TestGetAlwaysFiles(t *testing.T) {
	conf := &SetupConf{
		Files: []FileEntry{
			{Source: "/a", Dest: "/b", Mode: "newest"},
			{Source: "/c", Dest: "/d", Mode: "always"},
			{Source: "/e", Dest: "/f", Mode: "newest"},
			{Source: "/g", Dest: "/h", Mode: "always"},
		},
	}

	always := GetAlwaysFiles(conf)
	if len(always) != 2 {
		t.Errorf("expected 2 always files, got %d", len(always))
	}
}

func TestGetAlwaysFilesNil(t *testing.T) {
	always := GetAlwaysFiles(nil)
	if len(always) != 0 {
		t.Errorf("expected 0 always files for nil conf, got %d", len(always))
	}
}

func TestExpandHome(t *testing.T) {
	home, _ := os.UserHomeDir()

	tests := []struct {
		input string
		want  string
	}{
		{"~/.tmux.conf", filepath.Join(home, ".tmux.conf")},
		{"/absolute/path", "/absolute/path"},
		{"relative/path", "relative/path"},
	}

	for _, tt := range tests {
		got := expandHome(tt.input)
		if got != tt.want {
			t.Errorf("expandHome(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestExpandRemoteHome(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"~/.tmux.conf", "/home/sprite/.tmux.conf"},
		{"/absolute/path", "/absolute/path"},
	}

	for _, tt := range tests {
		got := expandRemoteHome(tt.input)
		if got != tt.want {
			t.Errorf("expandRemoteHome(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}
