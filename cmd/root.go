package cmd

import (
	"os"
	"strings"

	"github.com/spf13/cobra"
)

var (
	verbose bool
)

// rootCmd is the base command for sp.
var rootCmd = &cobra.Command{
	Use:   "sp [target]",
	Short: "Sprite Repository Manager - manage and monitor Fly.io sprite environments",
	Long: `sp creates isolated Fly.io sprite environments with file syncing,
session management, and a TUI dashboard for monitoring all your sprites.

Quick start:
  sp .                Connect to a sprite for the current directory
  sp owner/repo       Connect to a sprite for a GitHub repository
  sp tui              Open the TUI dashboard
  sp import <name>    Import an existing sprite into the dashboard`,
	// When called with positional args that don't match a subcommand,
	// treat them as connect targets (sp . / sp owner/repo)
	Args:               cobra.ArbitraryArgs,
	DisableFlagParsing: false,
	RunE:               runDefault,
	SilenceUsage:       true,
	SilenceErrors:      true,
}

// Execute runs the root command. Called from main.go.
func Execute() error {
	return rootCmd.Execute()
}

// runDefault handles the case where sp is called with a target but no subcommand.
// This lets `sp .` and `sp owner/repo` work without the `connect` prefix.
func runDefault(cmd *cobra.Command, args []string) error {
	if len(args) == 0 {
		return cmd.Help()
	}

	// Check if the first arg looks like it should be a connect target
	first := args[0]
	if first == "." || strings.HasPrefix(first, "./") || strings.HasPrefix(first, "/") || strings.Contains(first, "/") {
		// Inherit relevant flags from root invocation
		if v, _ := cmd.Flags().GetBool("no-sync"); v {
			noSync = true
		}
		if v, _ := cmd.Flags().GetString("name"); v != "" {
			sessionName = v
		}
		if v, _ := cmd.Flags().GetBool("web"); v {
			webMode = true
		}
		if v, _ := cmd.Flags().GetBool("web-proxy"); v {
			webProxy = true
		}
		if v, _ := cmd.Flags().GetInt("web-dev-port"); v != 0 {
			webDevPort = v
		}

		// Handle -- separator for exec command
		connectArgs := args
		for i, a := range args {
			if a == "--" {
				connectArgs = args[:i]
				if i+1 < len(args) {
					execCmd = strings.Join(args[i+1:], " ")
				}
				break
			}
		}

		return runConnect(connectCmd, connectArgs)
	}

	return cmd.Help()
}

func init() {
	rootCmd.PersistentFlags().BoolVarP(&verbose, "verbose", "v", false, "enable verbose output")

	// Allow connect-related flags on root too
	rootCmd.Flags().Bool("no-sync", false, "disable file syncing")
	rootCmd.Flags().String("name", "", "tmux session name")
	rootCmd.Flags().Bool("web", false, "enable opencode web UI via sprite service")
	rootCmd.Flags().Bool("web-proxy", false, "enable reverse proxy in front of opencode")
	rootCmd.Flags().Int("web-dev-port", 0, "development server port for proxy fallthrough")

	// Prevent cobra from complaining about unknown flags being passed through --
	rootCmd.TraverseChildren = true

	// Ensure that unknown subcommands are treated as connect targets
	rootCmd.SetHelpCommand(&cobra.Command{Hidden: true})
}

// handlePostRun is registered to set PATH-like default behavior.
// This allows `sp .` to be equivalent to `sp connect .`.
func init() {
	// Override the args function to intercept unknown subcommand errors
	// and redirect them to connect
	originalArgs := rootCmd.Args
	rootCmd.Args = func(cmd *cobra.Command, args []string) error {
		if originalArgs != nil {
			return originalArgs(cmd, args)
		}
		return nil
	}

	// Disable the suggestions for unknown commands since we handle them
	rootCmd.DisableSuggestions = true
}

// findExecSeparator returns the index of "--" in os.Args and the remaining args.
func findExecSeparator() (beforeArgs []string, execArgs string) {
	args := os.Args[1:]
	for i, a := range args {
		if a == "--" {
			before := args[:i]
			after := args[i+1:]
			return before, strings.Join(after, " ")
		}
	}
	return args, ""
}
