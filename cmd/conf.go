package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/jphenow/sp/internal/setup"
)

// confCmd manages the setup.conf file.
var confCmd = &cobra.Command{
	Use:   "conf",
	Short: "Manage sprite setup configuration",
}

// confInitCmd creates a default setup.conf file.
var confInitCmd = &cobra.Command{
	Use:   "init",
	Short: "Create a default setup.conf file",
	RunE: func(cmd *cobra.Command, args []string) error {
		confPath := setup.DefaultConfPath()

		// Check if it already exists
		if _, err := os.Stat(confPath); err == nil {
			fmt.Printf("setup.conf already exists at %s\n", confPath)
			return nil
		}

		// Create directory
		dir := filepath.Dir(confPath)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("creating config directory: %w", err)
		}

		// Write default config
		defaultConf := `# Sprite setup configuration
# Files and commands to run when setting up a new sprite or reconnecting.

[files]
# Copy dotfiles to sprite. Default mode is [newest] (only if local is newer).
# Use [always] to overwrite every time.
# Use source -> dest for different local/remote paths.
#
# ~/.tmux.conf
# ~/.config/opencode/config.toml [always]
# ~/.local/share/opencode/auth.json
# ~/.config/local -> ~/.config/remote

[commands]
# Conditional commands. Format: condition :: command
# Use ! prefix to negate condition (run when condition fails).
#
# ! command -v opencode :: curl -fsSL https://opencode.ai/install | bash &
# command -v npm :: npm install -g prettier
`
		if err := os.WriteFile(confPath, []byte(defaultConf), 0o644); err != nil {
			return fmt.Errorf("writing setup.conf: %w", err)
		}

		fmt.Printf("Created %s\n", confPath)
		return nil
	},
}

// confEditCmd opens setup.conf in the user's editor.
var confEditCmd = &cobra.Command{
	Use:   "edit",
	Short: "Edit the setup.conf file",
	RunE: func(cmd *cobra.Command, args []string) error {
		confPath := setup.DefaultConfPath()

		// Ensure the file exists
		if _, err := os.Stat(confPath); os.IsNotExist(err) {
			fmt.Println("No setup.conf found. Run 'sp conf init' first.")
			return nil
		}

		editor := os.Getenv("EDITOR")
		if editor == "" {
			editor = "vi"
		}

		editorCmd := exec.Command(editor, confPath)
		editorCmd.Stdin = os.Stdin
		editorCmd.Stdout = os.Stdout
		editorCmd.Stderr = os.Stderr
		return editorCmd.Run()
	},
}

// confShowCmd displays the current setup.conf.
var confShowCmd = &cobra.Command{
	Use:   "show",
	Short: "Show the current setup.conf",
	RunE: func(cmd *cobra.Command, args []string) error {
		confPath := setup.DefaultConfPath()

		data, err := os.ReadFile(confPath)
		if os.IsNotExist(err) {
			fmt.Println("No setup.conf found. Run 'sp conf init' to create one.")
			return nil
		}
		if err != nil {
			return fmt.Errorf("reading setup.conf: %w", err)
		}

		fmt.Print(string(data))
		return nil
	},
}

func init() {
	confCmd.AddCommand(confInitCmd, confEditCmd, confShowCmd)
	rootCmd.AddCommand(confCmd)
}
