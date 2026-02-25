package cmd

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"time"

	"github.com/spf13/cobra"

	"github.com/jphenow/sp/internal/daemon"
	"github.com/jphenow/sp/internal/logging"
	"github.com/jphenow/sp/internal/store"
)

// daemonCmd manages the background daemon process.
var daemonCmd = &cobra.Command{
	Use:   "daemon",
	Short: "Manage the sp background daemon",
}

// daemonStartCmd starts the daemon in the foreground (normally auto-started).
var daemonStartCmd = &cobra.Command{
	Use:   "start",
	Short: "Start the daemon (normally auto-started)",
	RunE: func(cmd *cobra.Command, args []string) error {
		config := daemon.DefaultConfig()

		if daemon.IsRunning(config) {
			fmt.Println("Daemon is already running")
			return nil
		}

		// Set up file + stderr logging so we can debug daemon issues
		if err := logging.SetupMulti(os.Stderr); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: log setup failed: %v\n", err)
		}
		defer logging.Close()

		db, err := store.Open()
		if err != nil {
			return fmt.Errorf("opening database: %w", err)
		}
		defer db.Close()

		d := daemon.New(config, db)
		fmt.Printf("Daemon starting (socket: %s, pid: %d, log: %s)\n",
			config.SocketPath, os.Getpid(), logging.DefaultLogPath())
		return d.Start(context.Background())
	},
}

// daemonStopCmd stops a running daemon.
var daemonStopCmd = &cobra.Command{
	Use:   "stop",
	Short: "Stop the running daemon",
	RunE: func(cmd *cobra.Command, args []string) error {
		config := daemon.DefaultConfig()
		if !daemon.IsRunning(config) {
			fmt.Println("Daemon is not running")
			return nil
		}

		// Connect and tell it to stop
		dc, err := daemon.ConnectTo(config.SocketPath)
		if err != nil {
			return fmt.Errorf("connecting to daemon: %w", err)
		}
		defer dc.Close()

		// Just close the connection - daemon will idle-stop eventually
		// For immediate stop, we'd need a shutdown RPC
		fmt.Println("Daemon will stop on idle timeout")
		return nil
	},
}

// daemonStatusCmd shows daemon status.
var daemonStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show daemon status",
	RunE: func(cmd *cobra.Command, args []string) error {
		config := daemon.DefaultConfig()
		if daemon.IsRunning(config) {
			dc, err := daemon.ConnectTo(config.SocketPath)
			if err != nil {
				fmt.Printf("Daemon running but not responding: %v\n", err)
				return nil
			}
			defer dc.Close()

			if err := dc.Ping(); err != nil {
				fmt.Printf("Daemon running but ping failed: %v\n", err)
				return nil
			}
			fmt.Println("Daemon is running and responsive")
			return nil
		}
		fmt.Println("Daemon is not running")
		return nil
	},
}

// daemonRestartCmd triggers a graceful restart of the running daemon.
// If the daemon isn't running, starts it fresh.
var daemonRestartCmd = &cobra.Command{
	Use:   "restart",
	Short: "Restart the daemon (picks up new binary)",
	Long: `Gracefully restarts the sp daemon. The daemon will:

1. Kill all managed proxy processes
2. Close its socket and PID file
3. Re-exec itself with the current binary on disk

This is useful after rebuilding sp to pick up code changes.
If the daemon is not running, it will be started fresh.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		config := daemon.DefaultConfig()
		if !daemon.IsRunning(config) {
			fmt.Println("Daemon is not running, starting fresh...")
			_, err := daemon.EnsureRunning()
			if err != nil {
				return fmt.Errorf("starting daemon: %w", err)
			}
			fmt.Println("Daemon started")
			return nil
		}

		dc, err := daemon.ConnectTo(config.SocketPath)
		if err != nil {
			return fmt.Errorf("connecting to daemon: %w", err)
		}
		defer dc.Close()

		if err := dc.Restart(); err != nil {
			return fmt.Errorf("restarting daemon: %w", err)
		}

		fmt.Println("Daemon restarting...")

		// Wait for the new daemon to come up
		for i := 0; i < 50; i++ {
			time.Sleep(200 * time.Millisecond)
			newDC, err := daemon.ConnectTo(config.SocketPath)
			if err == nil {
				if err := newDC.Ping(); err == nil {
					newDC.Close()
					fmt.Println("Daemon restarted successfully")
					return nil
				}
				newDC.Close()
			}
		}

		fmt.Println("Warning: daemon may not have restarted cleanly. Check 'sp daemon status'")
		return nil
	},
}

// daemonLogsCmd tails the daemon log file.
var daemonLogsCmd = &cobra.Command{
	Use:   "logs",
	Short: "Tail the daemon log file",
	Long: `Tails the sp daemon log at ~/.config/sp/sp.log.

Use -f to follow the log in real time (like tail -f).
Use -n to show only the last N lines (default 50).`,
	RunE: func(cmd *cobra.Command, args []string) error {
		logPath := logging.DefaultLogPath()
		if _, err := os.Stat(logPath); os.IsNotExist(err) {
			fmt.Printf("No log file found at %s\n", logPath)
			fmt.Println("The daemon hasn't been started yet, or logging isn't configured.")
			return nil
		}

		follow, _ := cmd.Flags().GetBool("follow")
		lines, _ := cmd.Flags().GetInt("lines")

		// Use tail command for simplicity — available on macOS and Linux
		tailArgs := []string{fmt.Sprintf("-n%d", lines)}
		if follow {
			tailArgs = append(tailArgs, "-f")
		}
		tailArgs = append(tailArgs, logPath)

		tailCmd := exec.Command("tail", tailArgs...)
		tailCmd.Stdout = os.Stdout
		tailCmd.Stderr = os.Stderr
		tailCmd.Stdin = os.Stdin
		return tailCmd.Run()
	},
}

func init() {
	daemonLogsCmd.Flags().BoolP("follow", "f", false, "follow the log output (like tail -f)")
	daemonLogsCmd.Flags().IntP("lines", "n", 50, "number of lines to show")

	daemonCmd.AddCommand(daemonStartCmd, daemonStopCmd, daemonRestartCmd, daemonStatusCmd, daemonLogsCmd)
	rootCmd.AddCommand(daemonCmd)
}
