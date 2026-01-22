# Chamber

Chamber is a tool for running coding agents like Claude or Codex inside [Tart](https://github.com/cirruslabs/tart) macOS virtual machines
with the current directory mounted. It provides a lightweight isolated environment for you agents in YOLO mode.

Don't think about prompt injection attacks anymore! Configure a macOS virtual machine only with YOLO-safe permissions
and run your agents inside it with Chamber.

![Chamber Illustration](assets/chamber-illustration.png)

## Features

- **VM Isolation Security**: Prevents prompt injection attacks by isolating AI agents in ephemeral VMs
- **Agent Safety**: Perfect for AI agents running with flags like `--dangerously-skip-permissions`, `--dangerously-bypass-approvals-and-sandbox`, or similar "YOLO" modes
- **Auto-Update Claude CLI**: Automatically updates Claude CLI to the latest version before each run (adds ~5-10 seconds)
- Run commands in isolated Tart VMs that are automatically destroyed after execution
- Automatic mounting of current directory
- Support for mounting additional directories with `--dir`

## Installation

First install `chamber` and initialize it so it will download (around 20GB) and setup a seed virtual machine for all future executions:

```bash
brew install --cask cirruslabs/cli/chamber
chamber init ghcr.io/cirruslabs/macos-sequoia-base:latest
```

This will create a `chamber-seed` Tart VM. You can customize the base image to your needs:

```base
tart run chamber-seed
```

## Why Use Chamber for AI Agents?

**Problem**: AI agents running with permissive flags like `--dangerously-skip-permissions`, `--dangerously-bypass-approvals-and-sandbox`, `--yes`, or `--auto-commits` are vulnerable to prompt injection attacks that can compromise your host system.

**Solution**: Chamber isolates AI agents in ephemeral VMs, making "YOLO" mode safe:

```bash
# ❌ DANGEROUS: Direct execution on host
claude --dangerously-skip-permissions
codex --dangerously-bypass-approvals-and-sandbox

# ✅ SAFE: Isolated execution in ephemeral VM (chamber will automatically add the dangerous flags for you)
chamber claude
chamber codex
```

**Key Benefits**:
- **Zero Host Risk**: Even if prompt injection succeeds, damage is contained in the VM
- **Automatic Cleanup**: VMs are destroyed after each run - always start from a clean seed image
- **Always Up-to-Date**: Claude CLI automatically updates to the latest version before each run
- **Full Functionality**: AI agents work normally but can't escape the sandbox
- **Easy Integration**: Just prefix your existing AI agent commands with `chamber`

## Mounting Additional Directories

By default, Chamber only mounts the current working directory. Use the `--dir` flag to mount additional host directories into the VM:

```bash
# Mount a single additional directory
chamber --dir=data:~/my-data claude

# Mount multiple directories
chamber --dir=memory:~/basic-memory --dir=config:~/.basic-memory claude

# Mount a directory as read-only
chamber --dir=reference:~/docs:ro claude
```

**Basic Format**: `--dir=name:hostpath[:ro]`
- `name`: Mount point name (must be unique and different from your current working directory's name)
- `hostpath`: Host path (supports `~` for home directory)
- `ro`: Optional, mount as read-only

Mounted directories are available at `~/workspace/<name>` inside the VM.

### Mounting to Custom VM Paths

You can also mount directories to specific locations in the VM (like `~/.claude` for API keys) using this syntax:

```bash
# Mount Claude config to ~/.claude in the VM (read-only, copied to writable location)
chamber --dir=~/.claude:~/.claude:ro claude

# Mount multiple config directories
chamber --dir=~/.claude:~/.claude:ro --dir=~/.config:~/.config:ro claude

# Mount to any custom path (read-write)
chamber --dir=/etc/myconfig:~/my-host-config claude
```

**Custom Path Format**: `--dir=vmpath:hostpath[:ro]`
- `vmpath`: Target path in the VM (starting with `/` or `~`)
- `hostpath`: Host path (supports `~` for home directory)
- `ro`: Optional, mount as read-only

**How it works**:
- **Without `:ro`**: Chamber creates a symlink from the VM path to the mounted directory. Writes in the VM affect the host.
- **With `:ro`**: Chamber copies the mounted directory to the VM path. The VM can read and write, but changes don't affect the host. Perfect for configs like `~/.claude` where tools need to write debug logs.

> **Tip**: Mount `~/.claude` in read-only mode to share your API key safely - the VM gets a writable copy but can't modify your host:
> ```bash
> chamber --dir=~/.claude:~/.claude:ro claude
> ```
>
> This allows Claude to write debug logs to `~/.claude/debug/` in the VM without affecting your host's `~/.claude/` directory.

> **Note**: Each mount must be unique. You cannot use the same mount twice or use a name that matches your current working directory's folder name.

## Shared Hostname for Host-VM Communication

When using plugins that need to connect to services running on your host machine (databases, APIs, etc.), you can use the `--shared-hostname` flag to configure a hostname that works from both the host and the VM:

```bash
# Configure a shared hostname
chamber --shared-hostname=dev.local claude

# Chamber will configure the VM and prompt you to add to your host's /etc/hosts:
echo '127.0.0.1 dev.local' | sudo tee -a /etc/hosts
```

**How it works**:
- On your host: `dev.local` resolves to `127.0.0.1` (localhost)
- In the VM: `dev.local` resolves to your host machine's IP

This allows plugins to use a single configuration (e.g., `DATABASE_URL=postgresql://user:pass@dev.local:5432/db`) that works whether running on the host or in the VM.

**Example**: Connect to host PostgreSQL from VM plugins:

```bash
# Start PostgreSQL on host
postgres -h 0.0.0.0 -p 5432

# Configure plugins to use dev.local
export DATABASE_URL="postgresql://user:pass@dev.local:5432/db"

# Run chamber with shared hostname
chamber --shared-hostname=dev.local claude

# Plugins can now reach your host's PostgreSQL at dev.local:5432
```

## License

This project is licensed under the AGPLv3. Tart is licensed under the Fair Source License which allow royalty free usage on
personal devices and work stations.
