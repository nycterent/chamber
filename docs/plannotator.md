# Plannotator Setup Guide

This guide explains how to set up [Plannotator](https://plannotator.ai) with Chamber to visually review and annotate Claude's plans before they execute.

## What is Plannotator?

Plannotator is a visual plan annotation tool for Claude Code. When Claude enters "plan mode" to design an implementation approach, Plannotator provides a web interface to review, annotate, and approve the plan before Claude proceeds with execution.

## Why Use Plannotator with Chamber?

When running Claude in Chamber's isolated VMs, Plannotator runs inside the VM but you need to access its web UI from your host browser. Chamber automatically detects Plannotator and sets up SSH port forwarding, so you can access `http://localhost:19432` on your host without manual SSH tunnel configuration.

## Prerequisites

- Chamber installed and initialized (`chamber init ghcr.io/cirruslabs/macos-sequoia-base:latest`)
- A `chamber-seed` VM ready for customization

## Installation

### Step 1: Install Plannotator in the Seed VM

Start the seed VM and install Plannotator:

```bash
tart run chamber-seed
```

Inside the VM, run:

```bash
curl -fsSL https://plannotator.ai/install.sh | bash
```

Verify installation:

```bash
which plannotator
# Should output: /Users/admin/.local/bin/plannotator (or similar)
```

### Step 2: Configure Environment Variables

Add these to `~/.zshrc` in the VM:

```bash
echo 'export PLANNOTATOR_REMOTE=1' >> ~/.zshrc
echo 'export PLANNOTATOR_PORT=19432' >> ~/.zshrc
source ~/.zshrc
```

| Variable | Purpose |
|----------|---------|
| `PLANNOTATOR_REMOTE=1` | Enables remote/headless mode for VM usage |
| `PLANNOTATOR_PORT` | Port for the web UI (default: 19432) |

### Step 3: Configure Claude Code Hooks

Create or edit `~/.claude/settings.json` in the VM:

```bash
mkdir -p ~/.claude
cat > ~/.claude/settings.json << 'EOF'
{
  "hooks": {
    "PermissionRequest": [
      {
        "matcher": "ExitPlanMode",
        "hooks": [{"type": "command", "command": "plannotator", "timeout": 1800}]
      }
    ]
  }
}
EOF
```

This configures Claude to run `plannotator` whenever it exits plan mode. The 1800-second timeout (30 minutes) gives you time to review complex plans.

### Step 4: Persist Changes

Shut down the VM to save changes to the seed image:

```bash
sudo shutdown -h now
```

All ephemeral VMs created by Chamber will now include Plannotator.

## Verification

After setup, verify everything works:

1. **Check detection**: Run `chamber claude` and look for:
   ```
   ✓ Plannotator detected: forwarding localhost:19432 → VM
   ```

2. **Test the workflow**: Ask Claude to do something that triggers plan mode, like:
   ```
   Implement a new feature that requires planning
   ```

3. **Access the UI**: When Claude calls ExitPlanMode, open `http://localhost:19432` in your browser.

## Usage Workflow

```
┌─────────────────────────────────────────────────────────────┐
│ 1. Run: chamber claude                                       │
│    → "✓ Plannotator detected: forwarding localhost:19432"   │
├─────────────────────────────────────────────────────────────┤
│ 2. Work with Claude...                                       │
│    → Claude decides to plan an implementation               │
├─────────────────────────────────────────────────────────────┤
│ 3. Claude enters plan mode                                   │
│    → Creates plan file, explores codebase                   │
├─────────────────────────────────────────────────────────────┤
│ 4. Claude calls ExitPlanMode                                 │
│    → Hook triggers plannotator                              │
│    → Plannotator starts HTTP server                         │
├─────────────────────────────────────────────────────────────┤
│ 5. Open http://localhost:19432 in your browser              │
│    → Review the plan                                        │
│    → Add annotations/comments                               │
│    → Approve or request changes                             │
├─────────────────────────────────────────────────────────────┤
│ 6. Claude receives your feedback and proceeds               │
└─────────────────────────────────────────────────────────────┘
```

## Troubleshooting

| Problem | Cause | Solution |
|---------|-------|----------|
| "Plannotator not detected" | Binary not in PATH | Ensure `~/.local/bin` is in PATH in `~/.zshrc` |
| "Connection reset" in browser | Plannotator not running yet | Wait for Claude to exit plan mode - it only starts when triggered |
| "Failed to parse hook event" | Running plannotator manually | Plannotator must be triggered by Claude's hook - it reads event data from stdin |
| Port already in use | Another process on 19432 | Change `PLANNOTATOR_PORT` to a different value (e.g., 19433) |
| Hook not triggering | Settings.json misconfigured | Verify JSON syntax and hook structure in `~/.claude/settings.json` |
| Changes lost after VM restart | Didn't shut down seed VM | Always use `sudo shutdown -h now` to persist changes to seed image |

### Verifying PATH Configuration

If Plannotator isn't detected, check the PATH in a login shell:

```bash
# Inside the VM
zsh -l -c 'echo $PATH'
zsh -l -c 'which plannotator'
```

Chamber uses login shells (`zsh -l -c`) for detection, so `~/.zshrc` must add the plannotator directory to PATH.

## How It Works

### Detection Flow

1. Chamber establishes SSH connection to the VM
2. `DetectPlannotator()` runs: `zsh -l -c 'which plannotator && echo ${PLANNOTATOR_PORT:-19432}'`
3. If found, creates `PortForwarder` for the detected port
4. Port forwarder listens on `127.0.0.1:PORT` on the host
5. Incoming connections are tunneled through SSH to `127.0.0.1:PORT` in the VM

### Hook Trigger Flow

1. Claude enters plan mode and creates a plan
2. Claude calls `ExitPlanMode` tool to request approval
3. Claude Code's hook system intercepts this
4. Hook executes: `plannotator` with event data piped to stdin
5. Plannotator parses the event, starts HTTP server
6. You access the UI via the forwarded port

### Port Forwarding Architecture

```
┌──────────────────┐     SSH Tunnel     ┌──────────────────┐
│   Host Machine   │ ←───────────────── │   Chamber VM     │
│                  │                    │                  │
│  Browser         │                    │  Plannotator     │
│  localhost:19432 │ ←── PortForwarder ←── localhost:19432│
└──────────────────┘                    └──────────────────┘
```

## Customization

### Using a Different Port

If port 19432 conflicts with another service:

```bash
# In VM's ~/.zshrc
export PLANNOTATOR_PORT=19500
```

Chamber will automatically detect and forward the new port.

### Multiple Plannotator Instances

Each Chamber VM gets its own port forwarding. If you run multiple `chamber claude` sessions simultaneously, they may conflict. Use different ports or run sessions sequentially.

## See Also

- [Chamber README](../README.md) - Main documentation
- [CLAUDE.md](../CLAUDE.md) - Quick reference for development
