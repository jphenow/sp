# Claude Code Agent Patterns for sp

This document describes how to effectively use Claude Code with the `sp` (Sprite Repository Manager) tool, including recommended agent patterns and workflows.

## Overview

The `sp` tool creates isolated Fly.io sprite environments with Claude Code pre-configured, making it ideal for:
- Working on GitHub repositories in clean, reproducible environments
- Testing changes without affecting your local machine
- Collaborating with Claude Code on complex codebases
- Running builds and tests in isolated containers

## Quick Start with Claude Code

Once you've connected to a sprite using `sp owner/repo` or `sp .`, authentication is set up and files are synced. Launch Claude Code with `-- claude`:

```bash
# Connect to a repository and open claude
sp superfly/flyctl -- claude

# Or open bash first (default), then run claude manually
sp superfly/flyctl
```

## Recommended Agent Patterns

### 1. Exploratory Analysis
When first examining a new codebase, ask Claude to:
- Map out the project structure
- Identify key components and their relationships
- Explain architectural patterns used
- Find the entry points and main execution flows

**Example prompts:**
- "Explore this codebase and explain the overall architecture"
- "Where does the authentication flow start in this application?"
- "What testing frameworks are used and where are the tests located?"

### 2. Feature Development
Leverage Claude's ability to work across multiple files:
- Plan the implementation strategy before coding
- Create or modify multiple related files in one session
- Update tests alongside implementation code
- Ensure consistency across the codebase

**Example prompts:**
- "Add a new command to handle X, following the existing patterns"
- "Implement feature Y with tests"
- "Refactor the authentication module to support OAuth"

### 3. Debugging and Troubleshooting
Use Claude to investigate and fix issues:
- Analyze error messages and stack traces
- Trace execution paths
- Identify root causes of bugs
- Suggest and implement fixes

**Example prompts:**
- "This error occurs when I run X. Help me debug it."
- "Why is this function returning unexpected results?"
- "Trace the execution path when this condition is triggered"

### 4. Code Review and Refactoring
Have Claude review and improve code quality:
- Identify code smells and anti-patterns
- Suggest performance optimizations
- Ensure consistency with project conventions
- Modernize legacy code

**Example prompts:**
- "Review this module for potential improvements"
- "Refactor this function to be more readable"
- "Are there any security issues in this code?"

### 5. Documentation
Generate and update documentation:
- Create inline code comments
- Write or update README files
- Document API endpoints
- Generate usage examples

**Example prompts:**
- "Add comprehensive comments to this module"
- "Update the README to reflect the new features"
- "Document the API endpoints in this file"

## Working with Sprites

### Environment Isolation
Each sprite provides:
- Fresh environment for each repository
- No conflicts with local dependencies
- Consistent build environment
- Easy cleanup (just delete the sprite)

### Authentication
The `sp` tool automatically configures:
- Claude Code OAuth token
- SSH keys for GitHub
- Git configuration
- Shell environment variables

### Syncing Local Changes
Use `sp .` to work with uncommitted local changes:
```bash
cd ~/projects/my-app
# Make some local changes
sp .
# Claude can now work with your uncommitted changes in the sprite
```

## Best Practices

### 1. Start with Exploration
Before making changes, have Claude explore the codebase to understand:
- Project structure and organization
- Existing patterns and conventions
- Testing approaches
- Build and deployment processes

### 2. Incremental Changes
Work in small, testable increments:
- Implement one feature at a time
- Run tests after each change
- Commit working code frequently
- Use Claude to help with git operations

### 3. Leverage Context
Claude has access to the entire repository:
- Ask about relationships between files
- Request consistency checks across the codebase
- Have Claude find similar implementations to follow

### 4. Use Sprites for Experimentation
Create sprites for experimental work:
```bash
# Try an experimental approach in isolation
sp myorg/myrepo
# Make experimental changes
# Delete the sprite if it doesn't work out
sprite delete gh-myorg--myrepo
```

### 5. Maintain Clean Sprites
When a sprite becomes cluttered:
- Delete it and create a fresh one
- The `sp` tool makes this trivial
- Fresh clones ensure clean state

### 6. Use `.sprite` Files for Consistency
If you run `sprite use <name>` in a directory, it creates a `.sprite` file that `sp` will respect:
```bash
# Set a specific sprite for this directory
sprite use my-custom-sprite

# Now sp will always use that sprite, regardless of git remote or directory name
sp .
```

This is useful when:
- You want a custom sprite name
- Multiple directories should share the same sprite
- The auto-detected name isn't what you want

## tmux Session Workflows

### Detach and Reattach

Each `sp` invocation runs its command inside a persistent tmux session. You can detach and reattach freely:

```bash
# Connect to a sprite (starts tmux session "claude")
sp owner/repo

# Inside the sprite, press Ctrl-b d to detach from tmux
# You'll return to your local terminal

# Reconnect later — reattaches to the same running session
sp owner/repo
```

The Claude Code process keeps running inside the tmux session even while you're detached. This is useful for long-running tasks — detach, come back later, and pick up where you left off.

**Note:** The session persists as long as the command inside it is running. If the command exits (e.g., `claude -c` with no conversation to continue), the session ends. Use interactive commands like `bash` or `claude` (without `-c`) for persistent sessions.

### Multiple Sessions in One Sprite

Run multiple independent commands in the same sprite simultaneously:

```bash
# Terminal 1: run claude
sp owner/repo --name claude-main

# Terminal 2: open a bash shell for manual debugging
sp owner/repo --cmd bash --name debug

# Terminal 3: run tests in a loop
sp owner/repo --cmd bash --name tests
```

Each `--name` creates a separate tmux session. Use `sp sessions owner/repo` to list them all.

### Custom Commands

Use `--cmd` to run something other than `claude`:

```bash
# Drop into a shell
sp . --cmd bash

# Open an editor
sp owner/repo --cmd vim

# Run any command
sp . --cmd "htop"
```

## Common Workflows

### Contributing to Open Source
```bash
# 1. Create sprite for the project
sp opensource/project

# 2. Ask Claude to explore the contribution guidelines
# 3. Implement your feature
# 4. Ask Claude to review and ensure it follows project conventions
# 5. Create a pull request
```

### Debugging Production Issues
```bash
# 1. Create sprite with the production code
sp myorg/production-app

# 2. Share error logs with Claude
# 3. Ask Claude to help investigate
# 4. Test fixes in the sprite environment
# 5. Apply fixes to production
```

### Learning a New Codebase
```bash
# 1. Create sprite for the repository
sp company/big-project

# 2. Ask Claude to provide an architectural overview
# 3. Request explanations of specific components
# 4. Have Claude create documentation as you learn
# 5. Experiment with changes to deepen understanding
```

## Tips and Tricks

### Multi-file Operations
Claude can work across multiple files simultaneously:
- "Update the authentication module and its tests"
- "Refactor this feature across all affected files"
- "Ensure consistency across the API layer"

### Iterative Refinement
Use Claude iteratively:
- Start with a broad request
- Ask follow-up questions
- Request refinements
- Have Claude explain its changes

### Code Generation with Context
Claude understands your codebase:
- "Add a new handler following the pattern in handlers/auth.go"
- "Create a test file for this module using the existing test patterns"
- "Add error handling consistent with the rest of the codebase"

### Shell Integration
Claude can help with shell commands in sprites:
- "Run the build and fix any compilation errors"
- "Set up the development environment"
- "Create a new git branch and commit these changes"

## Troubleshooting

### Token Issues
If Claude Code authentication fails:
```bash
# Regenerate the token
claude setup-token

# Update the saved token
echo "sk-ant-oat01-YOUR_NEW_TOKEN" > ~/.claude-token
chmod 600 ~/.claude-token
```

### Sprite Connection Issues
If you can't connect to a sprite:
```bash
# Check sprite status
sprite list

# Restart sprite if needed
sprite restart gh-owner--repo
```

### Sync Issues
If local changes aren't syncing properly:
```bash
# Delete and recreate the sprite
sprite delete gh-owner--repo
sp .  # Will create fresh sprite and sync
```

## Advanced Patterns

### CI/CD Integration
Use sprites to test CI/CD changes:
- Test build scripts in isolation
- Validate deployment procedures
- Debug CI failures locally

### Multi-repository Projects
Create sprites for related repositories:
```bash
sp myorg/frontend
sp myorg/backend
sp myorg/shared-lib
```

### Security Audits
Use Claude to review code for security issues:
- "Scan this codebase for SQL injection vulnerabilities"
- "Review the authentication implementation for security issues"
- "Check for proper input validation across the API"

## Resources

- [Claude Code Documentation](https://docs.anthropic.com/claude/docs/claude-code)
- [Fly.io Sprites Documentation](https://fly.io/docs/reference/sprites/)
- [GitHub CLI Documentation](https://cli.github.com/manual/)

## Contributing

If you discover useful agent patterns or workflows, please contribute them back to this document by opening a pull request.
