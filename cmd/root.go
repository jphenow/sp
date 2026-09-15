package cmd

import (
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
		if v, _ := cmd.Flags().GetDuration("keep-warm"); v != 0 {
			keepWarmDur = v
		}
		if v, _ := cmd.Flags().GetBool("remote-control"); v {
			remoteControl = true
		}
		if v, _ := cmd.Flags().GetBool("rc"); v {
			rcAlias = true
		}
		if v, _ := cmd.Flags().GetBool("no-hold"); v {
			noHold = true
		}

		// Handle -- separator for exec command (`sp . -- claude`).
		connectArgs, command := splitAtDash(args, cmd.ArgsLenAtDash())
		if command != "" {
			execCmd = command
		}

		return runConnect(connectCmd, connectArgs)
	}

	return cmd.Help()
}

func init() {
	rootCmd.PersistentFlags().BoolVarP(&verbose, "verbose", "v", false, "enable verbose output")

	// Allow connect-related flags on root too so `sp owner/repo --flag`
	// works without the `connect` prefix. Each flag registered here must
	// also be inherited in runDefault above.
	rootCmd.Flags().Bool("no-sync", false, "disable file syncing")
	rootCmd.Flags().String("name", "", "tmux session name")
	rootCmd.Flags().Bool("web", false, "enable opencode web UI via sprite service")
	rootCmd.Flags().Bool("web-proxy", false, "enable reverse proxy in front of opencode")
	rootCmd.Flags().Int("web-dev-port", 0, "development server port for proxy fallthrough")
	rootCmd.Flags().Duration("keep-warm", 0, "keep sprite warm after disconnect (e.g. 1h)")
	rootCmd.Flags().Bool("remote-control", false, "launch Claude with Remote Control (join from phone/browser)")
	rootCmd.Flags().Bool("rc", false, "alias for --remote-control")
	rootCmd.Flags().Bool("no-hold", false, "don't hold the sprite Active for the life of the session")

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

// splitAtDash splits positional args at the "--" separator into connect
// arguments and the command to run, joined with spaces.
//
// dashAt must come from cobra's cmd.ArgsLenAtDash(): cobra REMOVES the "--"
// token itself while parsing, so args never contain it and scanning them for
// "--" can't work. That's what used to happen — `sp . -- claude` arrived as
// [".", "claude"], "claude" was taken as a variant name, and sp created a new
// sprite called <name>--claude. dashAt is -1 when there was no separator.
func splitAtDash(args []string, dashAt int) (connectArgs []string, command string) {
	if dashAt < 0 || dashAt > len(args) {
		return args, ""
	}
	return args[:dashAt], strings.Join(args[dashAt:], " ")
}
