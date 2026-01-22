# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

Chamber is a macOS security tool that runs AI coding agents (Claude, Codex) inside isolated Tart VMs to prevent prompt injection attacks. It creates ephemeral VMs that automatically destroy themselves after execution, making "YOLO" mode (`--dangerously-skip-permissions`) safe by containing any potential damage within the VM.

## Common Commands

```bash
# Build all packages
go build ./...

# Build optimized static binary for distribution (no C dependencies)
CGO_ENABLED=0 go build -ldflags="-s -w" -o chamber

# Install locally built binary
sudo cp chamber /usr/local/bin/chamber

# Run linter (must pass before completing tasks)
golangci-lint run -v

# Run all tests
go test ./...

# Run a single test
go test -run TestFunctionName ./path/to/package
```

### Testing Chamber Locally

```bash
# Initialize chamber with a seed VM (downloads ~20GB)
chamber init ghcr.io/cirruslabs/macos-sequoia-base:latest

# Install Claude plugins into seed VM (persists to all ephemeral VMs)
chamber install-plugin @anthropic-ai/basic-memory

# Customize seed VM manually
tart run chamber-seed

# Run Claude/Codex in isolated VM
chamber claude
chamber codex
```

## Module

`github.com/cirruslabs/chamber` (Go 1.21+)

### Key Dependencies

- `github.com/spf13/cobra`: CLI framework
- `github.com/avast/retry-go/v4`: SSH connection retry logic
- `golang.org/x/crypto/ssh`: SSH client implementation
- `golang.org/x/term`: Terminal handling for PTY proxying

## Architecture

### Core Components

**VM Management** (`internal/vm/tart/`):
- `vm.go`: VM lifecycle management - creates ephemeral VMs with timestamp-based names (`chamber-ephemeral-YYYYMMDD-HHMMSS`)
- `cmd.go`: Tart CLI wrapper for cloning, starting, stopping VMs
- Uses `DirectoryMount` to expose host directories via virtiofs to the guest VM

**Command Execution** (`internal/executor/`):
- `executor.go`: Orchestrates command execution inside VMs
- Mounts working directory at `~/workspace/<dirname>` in guest using `mount_virtiofs`
- Two execution modes: `Execute()` for non-interactive, `ExecuteInteractive()` for full PTY

**SSH Communication** (`internal/ssh/`):
- `client.go`: SSH client with retry logic via `WaitForSSH()`
- `terminal.go`: Full PTY terminal proxying with window resize support (`SIGWINCH`)
- Handles both interactive (raw terminal) and non-interactive modes

**CLI Commands** (`internal/commands/`):
- `root.go`: Main command with backward compatibility for direct command execution; handles SIGINT/SIGTERM for graceful shutdown
- `claude.go`: Runs `claude` with `--dangerously-skip-permissions` auto-prepended; contains the shared `runCommand()` function used by all execution commands; parses `--dir` flags for additional mounts
- `codex.go`: Runs `codex` with `--dangerously-bypass-approvals-and-sandbox` auto-prepended
- `init.go`: Sets up `chamber-seed` VM by cloning remote image and installing `@anthropic-ai/claude-code`
- `install_plugin.go`: Installs Claude plugins into `chamber-seed` VM via npm (runs seed VM, installs, stops)

### Execution Flow

1. **VM Creation**: Clone `chamber-seed` → ephemeral VM with unique timestamp name
2. **VM Configuration**: Set random MAC address, configure CPU/memory if specified
3. **VM Start**: Launch VM with `--no-graphics --no-clipboard --no-audio` and directory mounts
4. **SSH Wait**: Poll for VM IP address (30s timeout) and establish SSH connection
5. **Mount Working Directory**: Unmount default shares, mount virtiofs at `~/workspace`
6. **Update Claude CLI** (claude command only): Run `npm install -g @anthropic-ai/claude-code@latest` to get latest version
7. **Command Execution**: Execute command in mounted working directory via SSH
8. **Cleanup**: Stop VM (5s timeout), delete ephemeral VM on exit

### Key Design Decisions

- **Ephemeral VMs**: Fresh VM clone per execution prevents state leakage
- **Auto-Update Claude CLI**: Updates to latest version before each run (~5-10 seconds overhead)
- **virtiofs Mounting**: Host directories exposed to guest via Tart's virtiofs support
- **Login Shell**: Commands run via `zsh -l -c` to load user profile/environment
- **Default Credentials**: SSH uses `admin:admin` (seed VM must have these)
- **Signal Handling**: SIGINT/SIGTERM triggers graceful cleanup (VM stop + delete)

## Directory Mounting

The `--dir` flag supports two syntaxes:

1. **Simple mount**: `--dir=name:hostpath[:ro]` - mounts to `~/workspace/<name>` in VM
2. **Custom VM path**: `--dir=vmpath:hostpath[:ro]` - where vmpath starts with `/` or `~`

**Implementation details for custom paths**:
- **Read-write** (no `:ro`): Creates symlinks from VM path to `~/workspace/<mount-name>`
- **Read-only** (with `:ro`): Copies directory using `rsync -rltD` (handles symlinks in node_modules correctly)

Common use case for sharing API keys without host modification:
```bash
chamber --dir=~/.claude:~/.claude:ro claude
```

## Shared Hostname for Host-VM Communication

The `--shared-hostname` flag configures a hostname that resolves differently depending on context:
- **On the host**: Resolves to `127.0.0.1` (localhost)
- **In the VM**: Resolves to the host machine's IP

This allows plugins and services to use a single configuration that works from both environments.

### Usage

```bash
# Configure dev.local as shared hostname
chamber --shared-hostname=dev.local claude

# The VM automatically configures /etc/hosts to point dev.local to the host IP
# You'll be prompted to add this to your host's /etc/hosts:
echo '127.0.0.1 dev.local' | sudo tee -a /etc/hosts
```

### Common Use Cases

**Database connections**: Configure plugins to connect to `dev.local:5432` instead of `localhost:5432`:
```bash
# Services on host bind to localhost
postgres -h 0.0.0.0 -p 5432

# Plugin config uses dev.local
DATABASE_URL=postgresql://user:pass@dev.local:5432/db

# Works from host (dev.local → 127.0.0.1) and VM (dev.local → host IP)
```

**API servers**: Run development API server on host, access from VM:
```bash
# Start API on host
npm start  # Listening on localhost:3000

# Plugins in VM can reach it at dev.local:3000
chamber --shared-hostname=dev.local claude
```

### Implementation Details

When `--shared-hostname` is specified:
1. Chamber finds the default gateway in the VM (which is the host machine in Tart VMs)
2. Adds an entry to the VM's `/etc/hosts`: `<host-ip> <hostname>`
3. Prints instructions for configuring the host's `/etc/hosts` with `127.0.0.1 <hostname>`

The hostname persists for the lifetime of the ephemeral VM but is destroyed on VM cleanup.

**Technical note**: The host IP is discovered by finding the VM's default gateway using `netstat -nr`.

## Testing

Test files: `internal/ssh/terminal_test.go`, `internal/commands/claude_test.go`

Use standard `go test` - no custom test framework.

## Troubleshooting

### Binary Killed on macOS (exit code 137)

If the chamber binary is killed immediately after execution on a different Mac:

1. **Remove quarantine attribute**: `xattr -d com.apple.quarantine chamber`
2. **Build static binary**: Use `CGO_ENABLED=0` to eliminate C library dependencies that may differ between systems
3. **Strip symbols**: Use `-ldflags="-s -w"` to remove symbol tables that can cause issues

### Claude CLI Auto-Update Fails (exit code 127)

If you see `Warning: failed to update Claude CLI (continuing anyway): Process exited with status 127`, it means `npm` is not available in the seed VM's PATH:

**Cause**: Exit code 127 means "command not found" - the auto-update feature runs `npm install -g @anthropic-ai/claude-code@latest` but can't find `npm`.

**Solutions**:
1. **Ensure npm is in PATH**: Customize the seed VM to make sure npm is accessible in login shells:
   ```bash
   tart run chamber-seed
   # Inside the VM, verify npm is available:
   npm --version
   # If not, install Node.js or fix your shell profile (~/.zshrc)
   ```

2. **Re-run chamber init**: If you previously ran `chamber init`, it should have installed Claude Code via npm. If that step succeeded but auto-update fails, your shell profile might not be loading correctly for non-interactive commands.

**Note**: The auto-update failure is non-fatal - Chamber will continue using the version of Claude CLI installed in the seed VM. The warning is just informing you that it couldn't update to the latest version.

### Extended Attribute Errors with node_modules

When mounting directories containing `node_modules` (like `~/.claude` with installed plugins), the copy operation may encounter "Too many levels of symbolic links" errors. This is expected behavior:

- Node.js packages create symlinks in `.bin/` directories
- The rsync copy command uses `-rltD` flags to handle symlinks correctly
- Symlinks are preserved as symlinks in the VM copy, not dereferenced
- This is safe because the copy is isolated to the ephemeral VM
