package cmd

import (
	"context"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/jphenow/sp/internal/daemon"
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

		db, err := store.Open()
		if err != nil {
			return fmt.Errorf("opening database: %w", err)
		}
		defer db.Close()

		d := daemon.New(config, db)
		fmt.Printf("Daemon starting (socket: %s, pid: %d)\n", config.SocketPath, os.Getpid())
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

func init() {
	daemonCmd.AddCommand(daemonStartCmd, daemonStopCmd, daemonStatusCmd)
	rootCmd.AddCommand(daemonCmd)
}
