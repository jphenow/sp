package setup

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/jphenow/sp/internal/sprite"
)

// SetupConf represents a parsed setup.conf file with file entries and commands.
type SetupConf struct {
	Files    []FileEntry
	Commands []CommandEntry
}

// FileEntry represents a single [files] entry in setup.conf.
type FileEntry struct {
	Source string // local path (expanded)
	Dest   string // remote path (expanded)
	Mode   string // "newest" (default) or "always"
}

// CommandEntry represents a single [commands] entry in setup.conf.
type CommandEntry struct {
	Condition string // command to check (must succeed for Command to run)
	Command   string // command to execute if condition passes
}

// DefaultConfPath returns the default path for setup.conf.
func DefaultConfPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "sprite", "setup.conf")
}

// ParseSetupConf reads and parses the setup.conf file.
// Returns nil with no error if the file doesn't exist.
func ParseSetupConf(path string) (*SetupConf, error) {
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("opening setup.conf: %w", err)
	}
	defer f.Close()

	conf := &SetupConf{}
	section := ""
	scanner := bufio.NewScanner(f)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())

		// Skip empty lines and comments
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		// Section headers
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = strings.ToLower(strings.Trim(line, "[]"))
			continue
		}

		switch section {
		case "files":
			entry, err := parseFileEntry(line)
			if err != nil {
				return nil, fmt.Errorf("parsing file entry %q: %w", line, err)
			}
			conf.Files = append(conf.Files, entry)

		case "commands":
			entry, err := parseCommandEntry(line)
			if err != nil {
				return nil, fmt.Errorf("parsing command entry %q: %w", line, err)
			}
			conf.Commands = append(conf.Commands, entry)
		}
	}

	return conf, scanner.Err()
}

// parseFileEntry parses a single file line from setup.conf.
// Supports: path, path [always], source -> dest, source -> dest [always]
func parseFileEntry(line string) (FileEntry, error) {
	mode := "newest"

	// Check for [always] annotation
	if strings.HasSuffix(line, "[always]") {
		mode = "always"
		line = strings.TrimSpace(strings.TrimSuffix(line, "[always]"))
	} else if strings.HasSuffix(line, "[newest]") {
		line = strings.TrimSpace(strings.TrimSuffix(line, "[newest]"))
	}

	// Check for source -> dest syntax
	if idx := strings.Index(line, " -> "); idx != -1 {
		source := strings.TrimSpace(line[:idx])
		dest := strings.TrimSpace(line[idx+4:])
		return FileEntry{
			Source: expandHome(source),
			Dest:   expandRemoteHome(dest),
			Mode:   mode,
		}, nil
	}

	// Same path for source and dest
	expanded := expandHome(line)
	return FileEntry{
		Source: expanded,
		Dest:   expandRemoteHome(line),
		Mode:   mode,
	}, nil
}

// parseCommandEntry parses a command line from setup.conf.
// Format: condition :: command  or  ! condition :: command
func parseCommandEntry(line string) (CommandEntry, error) {
	parts := strings.SplitN(line, " :: ", 2)
	if len(parts) != 2 {
		// Simple command with no condition
		return CommandEntry{Command: line}, nil
	}
	return CommandEntry{
		Condition: strings.TrimSpace(parts[0]),
		Command:   strings.TrimSpace(parts[1]),
	}, nil
}

// expandHome replaces ~ with the actual home directory for local paths.
func expandHome(path string) string {
	if strings.HasPrefix(path, "~/") {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, path[2:])
	}
	return path
}

// expandRemoteHome replaces ~ with the sprite home directory for remote paths.
func expandRemoteHome(path string) string {
	if strings.HasPrefix(path, "~/") {
		return "/home/sprite/" + path[2:]
	}
	return path
}

// RunSetupConf executes a parsed setup.conf against a sprite.
func RunSetupConf(client *sprite.Client, spriteName string, conf *SetupConf) error {
	if conf == nil {
		return nil
	}

	// Process files
	for _, f := range conf.Files {
		if err := copySetupFile(client, spriteName, f); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to copy %s: %v\n", f.Source, err)
		}
	}

	// Process commands
	for _, c := range conf.Commands {
		if err := runSetupCommand(client, spriteName, c); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to run command: %v\n", err)
		}
	}

	return nil
}

// copySetupFile uploads a single file to the sprite, respecting the mode.
func copySetupFile(client *sprite.Client, spriteName string, entry FileEntry) error {
	// Check if source exists locally
	if _, err := os.Stat(entry.Source); err != nil {
		return fmt.Errorf("source file %q not found: %w", entry.Source, err)
	}

	// In "newest" mode, check remote mtime
	if entry.Mode == "newest" {
		// Get local mtime
		localInfo, err := os.Stat(entry.Source)
		if err != nil {
			return err
		}
		localMtime := localInfo.ModTime().Unix()

		// Get remote mtime
		out, err := client.Exec(sprite.ExecOptions{
			Sprite:  spriteName,
			Command: []string{"stat", "-c", "%Y", entry.Dest},
		})
		if err == nil {
			remoteMtime := strings.TrimSpace(string(out))
			if remoteMtime != "" {
				var remoteTime int64
				if _, err := fmt.Sscanf(remoteMtime, "%d", &remoteTime); err == nil {
					if remoteTime >= localMtime {
						return nil // Remote is same or newer, skip
					}
				}
			}
		}
		// If remote stat fails, file doesn't exist remotely - proceed with copy
	}

	// Ensure remote directory exists
	remoteDir := filepath.Dir(entry.Dest)
	if _, err := client.Exec(sprite.ExecOptions{
		Sprite:  spriteName,
		Command: []string{"mkdir", "-p", remoteDir},
	}); err != nil {
		return fmt.Errorf("creating remote directory %q: %w", remoteDir, err)
	}

	// Upload the file
	if _, err := client.Exec(sprite.ExecOptions{
		Sprite:  spriteName,
		Command: []string{"true"},
		Files:   map[string]string{entry.Source: entry.Dest},
	}); err != nil {
		return fmt.Errorf("uploading %q to %q: %w", entry.Source, entry.Dest, err)
	}

	// Preserve executable bit
	localInfo, err := os.Stat(entry.Source)
	if err == nil && localInfo.Mode()&0o111 != 0 {
		if _, err := client.Exec(sprite.ExecOptions{
			Sprite:  spriteName,
			Command: []string{"chmod", "+x", entry.Dest},
		}); err != nil {
			return fmt.Errorf("setting executable bit on %q: %w", entry.Dest, err)
		}
	}

	return nil
}

// runSetupCommand executes a conditional command on the sprite.
func runSetupCommand(client *sprite.Client, spriteName string, entry CommandEntry) error {
	// Check condition if present
	if entry.Condition != "" {
		conditionCmd := entry.Condition
		negate := false

		// Handle ! prefix (run command when condition fails)
		if strings.HasPrefix(conditionCmd, "! ") {
			negate = true
			conditionCmd = strings.TrimPrefix(conditionCmd, "! ")
		}

		_, err := client.Exec(sprite.ExecOptions{
			Sprite:  spriteName,
			Command: []string{"sh", "-c", conditionCmd},
		})
		conditionMet := err == nil
		if negate {
			conditionMet = !conditionMet
		}

		if !conditionMet {
			return nil // Condition not met, skip
		}
	}

	// Run the command
	if _, err := client.Exec(sprite.ExecOptions{
		Sprite:  spriteName,
		Command: []string{"sh", "-c", entry.Command},
	}); err != nil {
		return fmt.Errorf("running command %q: %w", entry.Command, err)
	}

	return nil
}

// GetAlwaysFiles returns only files with mode "always" from a setup.conf.
func GetAlwaysFiles(conf *SetupConf) []FileEntry {
	if conf == nil {
		return nil
	}
	var always []FileEntry
	for _, f := range conf.Files {
		if f.Mode == "always" {
			always = append(always, f)
		}
	}
	return always
}

// ConfModifiedSince checks if setup.conf has been modified since the given time.
func ConfModifiedSince(confPath string, since int64) bool {
	info, err := os.Stat(confPath)
	if err != nil {
		return false
	}
	return info.ModTime().Unix() > since
}
