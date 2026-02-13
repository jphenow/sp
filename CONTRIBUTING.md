# Contributing to sp

Thank you for your interest in contributing to `sp` (Sprite Repository Manager)! This document provides guidelines for contributing to the project.

## Getting Started

1. Fork the repository on GitHub
2. Clone your fork locally:
   ```bash
   git clone https://github.com/YOUR_USERNAME/sp.git
   cd sp
   ```
3. Create a branch for your changes:
   ```bash
   git checkout -b feature/your-feature-name
   ```

## Development Workflow

### Using sp to Develop sp

You can use `sp` itself to develop `sp`:

```bash
# Work with your local changes
cd /path/to/sp
sp .

# Claude Code will open in a sprite with your local changes
# Make changes, test them, and iterate
```

This gives you an isolated environment to test your changes without affecting your local `sp` installation.

### Testing Your Changes

Before submitting a pull request:

1. Test the basic workflows:
   ```bash
   # Test repo mode
   ./sp test-owner/test-repo

   # Test current directory mode
   cd /path/to/some/repo
   /path/to/your/sp .
   ```

2. Test authentication flows:
   - Delete `~/.claude-token` and test first-time setup
   - Test with missing SSH keys
   - Test with existing sprites

3. Test error handling:
   - Invalid repository names
   - Non-existent repositories
   - Network failures
   - Permission issues

4. Test cleanup:
   - Verify no temp files are left behind
   - Check that sprites are properly configured

## Code Style

### Shell Script Guidelines

- Use `bash` (not `sh`) for the script
- Set strict error handling: `set -euo pipefail`
- Use meaningful function names with `snake_case`
- Add comments for complex logic
- Use `local` for function-scoped variables
- Quote all variable expansions: `"$var"`
- Use `[[` for conditionals instead of `[`
- Prefer `${var:-default}` for default values

### Example

```bash
# Good: Clear function name, proper quoting, local variables
setup_authentication() {
    local sprite_name="$1"
    local token="${2:-}"

    if [[ -z "$token" ]]; then
        error "Token is required"
    fi

    info "Setting up authentication for: $sprite_name"
}
```

## Adding New Features

### Feature Proposal

For significant features, please:
1. Open an issue first to discuss the proposal
2. Explain the use case and expected behavior
3. Get feedback before implementing

### Implementation Guidelines

- Follow existing patterns in the codebase
- Add appropriate error handling
- Include user-facing messages (using `info`, `warn`, `error`)
- Update the README.md if adding user-visible features
- Update AGENTS.md if the feature affects Claude Code workflows

## Documentation

When adding features or making changes:

1. Update `README.md`:
   - Add new features to the Features section
   - Add new usage examples if applicable
   - Update troubleshooting section if relevant

2. Update `AGENTS.md`:
   - Add new agent patterns if applicable
   - Update workflows that are affected
   - Add tips for using the new feature with Claude Code

3. Update code comments:
   - Add comments to complex functions
   - Document function parameters and return values
   - Explain non-obvious logic

## Error Handling

- Use the `error()` function for fatal errors
- Use the `warn()` function for non-fatal issues
- Use the `info()` function for informational messages
- Provide clear, actionable error messages
- Include relevant context in error messages

Example:
```bash
# Bad
error "Failed"

# Good
error "Failed to create sprite '$sprite_name'. Check that you're authenticated with 'sprite list'"
```

## Authentication and Security

When working with authentication:
- Never log tokens or sensitive data
- Use environment variables for tokens
- Set appropriate file permissions (600 for secrets)
- Validate token format before saving
- Handle token errors gracefully

## Sprite Management

When modifying sprite-related code:
- Always check if a sprite exists before creating
- Wait for sprites to be ready before using
- Handle sprite creation failures gracefully
- Clean up temporary files and resources
- Use absolute paths for file uploads

## Pull Request Process

1. **Create a Pull Request**:
   - Provide a clear title and description
   - Reference any related issues
   - Explain what changes were made and why

2. **PR Description Should Include**:
   - What problem does this solve?
   - How was it tested?
   - Any breaking changes?
   - Screenshots/examples if applicable

3. **Review Process**:
   - Address review feedback
   - Keep the PR focused and reasonably sized
   - Rebase on main if needed

4. **Before Merging**:
   - Ensure all discussions are resolved
   - Verify tests pass
   - Update documentation if needed

## Commit Messages

Use clear, descriptive commit messages:

```bash
# Good
git commit -m "Add support for custom SSH key paths"
git commit -m "Fix token validation for wrapped input"
git commit -m "Update README with troubleshooting steps"

# Less helpful
git commit -m "Fix bug"
git commit -m "Update docs"
git commit -m "Changes"
```

## Testing Scenarios

Test these scenarios before submitting:

### New Sprite Creation
- [ ] Create sprite for first time
- [ ] Verify authentication setup
- [ ] Verify SSH keys copied
- [ ] Verify git config copied
- [ ] Verify repository cloned

### Existing Sprite
- [ ] Connect to existing sprite
- [ ] Verify repository updates (git pull)
- [ ] Verify auth files not re-copied unnecessarily

### Setup Config
- [ ] `sp conf init` creates starter config
- [ ] `sp conf edit` opens in $EDITOR
- [ ] `sp conf show` prints contents
- [ ] `[files]` entries copy to sprite on first connect
- [ ] `source -> dest` syntax maps different local/remote paths
- [ ] `[commands]` entries run when condition succeeds
- [ ] Editing setup.conf re-triggers setup on next connect

### Current Directory Mode
- [ ] Sync local directory
- [ ] Verify exclusions work (node_modules, .next, etc.)
- [ ] Verify `.git` is included in sync
- [ ] Verify git repository state maintained
- [ ] Test with uncommitted changes

### Error Conditions
- [ ] Invalid repository format
- [ ] Non-existent repository
- [ ] Missing SSH key
- [ ] Missing Claude token
- [ ] Sprite creation failure
- [ ] Network timeout

## Getting Help

If you need help:
- Open an issue with your question
- Check existing issues for similar questions
- Review the README.md and AGENTS.md documentation

## Code of Conduct

- Be respectful and constructive
- Welcome newcomers and help them get started
- Focus on the technical merits of contributions
- Assume good intentions

## License

By contributing to `sp`, you agree that your contributions will be licensed under the MIT License.

## Questions?

Feel free to open an issue if you have questions about contributing!
