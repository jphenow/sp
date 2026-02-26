# Claude Code Agent Patterns for sp

This document describes how to effectively use Claude Code with the `sp` tool inside sprite environments. For installation, usage, and command reference, see [README.md](README.md).

## Overview

The `sp` tool creates isolated Fly.io sprite environments with Claude Code pre-configured. See the [README quickstart](README.md#quickstart) for setup instructions.

## Quick Start with Claude Code

```bash
# Connect to a repository and open claude
sp superfly/flyctl -- claude

# Or boot the web UI
sp . --web
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

### 5. Documentation
Generate and update documentation:
- Create inline code comments
- Document API endpoints
- Generate usage examples

## Working with Sprites

### Environment Isolation
Each sprite provides:
- Fresh environment for each repository
- No conflicts with local dependencies
- Consistent build environment
- Easy cleanup (just delete the sprite)

### What sp auto-configures
See [Setup Configuration](README.md#setup-configuration) in the README.

### File Sync
See [File Sync](README.md#file-sync) in the README for details on bidirectional sync, `.gitignore` handling, conflict resolution, and the daemon lifecycle.

## Best Practices

### 1. Start with Exploration
Before making changes, have Claude explore the codebase to understand the project structure, patterns, testing approach, and build process.

### 2. Incremental Changes
Work in small, testable increments. Commit working code frequently. Use Claude to help with git operations.

### 3. Leverage Context
Claude has access to the entire repository. Ask about relationships between files, request consistency checks, and have Claude find similar implementations to follow.

### 4. Use Sprites for Experimentation
Create sprites for experimental work:
```bash
sp myorg/myrepo
# Make experimental changes
# Delete the sprite if it doesn't work out
```

### 5. Multiple Sessions
Run Claude and bash side by side:
```bash
sp . -- claude --name claude-main    # Terminal 1
sp . --name debug                    # Terminal 2
```

Use `sp sessions .` to list all active tmux sessions.

## Common Workflows

### Contributing to Open Source
```bash
sp opensource/project          # Create sprite
# Ask Claude to explore contribution guidelines
# Implement feature, review, create PR
```

### Learning a New Codebase
```bash
sp company/big-project         # Create sprite
# Ask Claude for architectural overview
# Request explanations of specific components
```

### Debugging Production Issues
```bash
sp myorg/production-app        # Create sprite
# Share error logs with Claude
# Investigate and test fixes in isolation
```

## Troubleshooting

See [Troubleshooting](README.md#troubleshooting) in the README.

## Resources

- [README.md](README.md) — Installation, usage, command reference
- [Sprites Documentation](https://sprites.dev)
- [Claude Code Documentation](https://docs.anthropic.com/en/docs/claude-code)
