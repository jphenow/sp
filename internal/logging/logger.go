package logging

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
)

// DefaultLogPath returns the path to the sp log file.
func DefaultLogPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "sp", "sp.log")
}

// logFile holds the open log file so we can close it on shutdown.
var logFile *os.File

// Setup initialises the global slog logger to write JSON lines to the sp log
// file. The file is opened in append mode and created if it doesn't exist.
// Call Close() to flush and close the file handle.
func Setup() error {
	path := DefaultLogPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("creating log directory: %w", err)
	}

	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("opening log file %s: %w", path, err)
	}
	logFile = f

	handler := slog.NewJSONHandler(f, &slog.HandlerOptions{
		Level: slog.LevelDebug,
	})
	slog.SetDefault(slog.New(handler))
	return nil
}

// SetupMulti initialises the global slog logger to write to both the log file
// and the provided writer (typically os.Stderr for foreground daemon mode).
func SetupMulti(w io.Writer) error {
	path := DefaultLogPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("creating log directory: %w", err)
	}

	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("opening log file %s: %w", path, err)
	}
	logFile = f

	multi := io.MultiWriter(f, w)
	handler := slog.NewJSONHandler(multi, &slog.HandlerOptions{
		Level: slog.LevelDebug,
	})
	slog.SetDefault(slog.New(handler))
	return nil
}

// LogFile returns the open file handle for the log file, or nil if not set up.
// Used by EnsureRunning to redirect daemon stdout/stderr.
func LogFile() *os.File {
	return logFile
}

// Close flushes and closes the log file.
func Close() {
	if logFile != nil {
		logFile.Sync()
		logFile.Close()
		logFile = nil
	}
}
