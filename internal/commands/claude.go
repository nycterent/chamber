package commands

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/cirruslabs/chamber/internal/executor"
	"github.com/cirruslabs/chamber/internal/ssh"
	"github.com/cirruslabs/chamber/internal/vm/tart"
	"github.com/spf13/cobra"
)

// parseDirectoryMounts parses --dir flag values into DirectoryMount structs.
// Format: name:hostpath[:ro] OR vmpath:hostpath[:ro]
// Examples:
//   - data:~/my-data              # mounts to ~/workspace/data
//   - docs:/path/to/docs:ro       # mounts to ~/workspace/docs (read-only)
//   - ~/.claude:~/.claude:ro      # mounts to ~/.claude in VM (read-only)
//   - ~/.config:~/.config         # mounts to ~/.config in VM
//
// Note: ~username syntax is not supported, only ~ for current user's home directory.
// If first part starts with / or ~, it's treated as a VM path and a symlink is created.
func parseDirectoryMounts(dirs []string) ([]tart.DirectoryMount, error) {
	var mounts []tart.DirectoryMount
	seenNames := make(map[string]bool)
	mountIndex := 0

	for _, dir := range dirs {
		parts := strings.Split(dir, ":")
		if len(parts) < 2 {
			return nil, fmt.Errorf("invalid --dir format: %q (expected name:hostpath[:ro] or vmpath:hostpath[:ro])", dir)
		}

		firstPart := strings.TrimSpace(parts[0])
		if firstPart == "" {
			return nil, fmt.Errorf("invalid --dir format: %q (mount name cannot be empty)", dir)
		}

		var name, vmPath string
		hostPath := parts[1]

		// Check if first part is a VM path (starts with / or ~)
		if strings.HasPrefix(firstPart, "/") || strings.HasPrefix(firstPart, "~") {
			// This is a VM path - generate a unique mount name
			vmPath = firstPart
			name = fmt.Sprintf("mount-%d", mountIndex)
			mountIndex++
		} else {
			// This is a mount name
			name = firstPart
		}

		// Check for duplicate mount names
		if seenNames[name] {
			return nil, fmt.Errorf("duplicate mount name: %q", name)
		}
		seenNames[name] = true

		// Expand ~ or ~/... in host path to the current user's home directory.
		// Note: forms like ~username/... are not supported.
		if hostPath == "~" || strings.HasPrefix(hostPath, "~/") {
			home, err := os.UserHomeDir()
			if err != nil {
				return nil, fmt.Errorf("failed to get home directory: %w", err)
			}
			if hostPath == "~" {
				hostPath = home
			} else {
				hostPath = filepath.Join(home, hostPath[2:])
			}
		}

		// Convert to absolute path
		absPath, err := filepath.Abs(hostPath)
		if err != nil {
			return nil, fmt.Errorf("failed to get absolute path for %q: %w", hostPath, err)
		}

		// Check if read-only (only "ro" is valid)
		readOnly := false
		if len(parts) > 2 {
			if parts[2] == "ro" {
				readOnly = true
			} else if parts[2] != "" {
				return nil, fmt.Errorf("invalid --dir format: %q (third parameter must be 'ro' for read-only, got %q)", dir, parts[2])
			}
		}

		mounts = append(mounts, tart.DirectoryMount{
			Name:     name,
			Path:     absPath,
			ReadOnly: readOnly,
			VMPath:   vmPath,
		})
	}
	return mounts, nil
}

func NewClaudeCmd() *cobra.Command {
	var (
		vmImage string
	)

	cmd := &cobra.Command{
		Use:   "claude [flags] [claude-args...]",
		Short: "Run claude in an isolated Tart VM with --dangerously-skip-permissions",
		Long: `Run claude inside an ephemeral Tart virtual machine with the current directory mounted.
Automatically prepends --dangerously-skip-permissions to claude arguments for AI agent execution.

Example:
  chamber claude
  chamber claude --model=opus
  chamber claude --vm=macos-xcode`,
		RunE: func(cmd *cobra.Command, args []string) error {
			// Prepend claude command and --dangerously-skip-permissions flag
			claudeArgs := []string{"claude", "--dangerously-skip-permissions"}
			claudeArgs = append(claudeArgs, args...)
			return runCommand(cmd.Context(), vmImage, 0, 0, "admin", "admin", additionalDirs, sharedHostname, true, claudeArgs)
		},
	}

	cmd.Flags().StringVar(&vmImage, "vm", "chamber-seed", "Tart VM image to use (default: chamber-seed)")

	// Stop parsing flags after the first non-flag argument AND disable flag parsing entirely for unknown flags
	cmd.Flags().SetInterspersed(false)
	cmd.DisableFlagParsing = false
	cmd.FParseErrWhitelist.UnknownFlags = true

	return cmd
}

func runCommand(ctx context.Context, vmImage string, cpuCount, memoryMB uint32, sshUser, sshPass string, extraDirs []string, sharedHostname string, interactive bool, args []string) error {
	// Check if Tart is installed
	if !tart.Installed() {
		return fmt.Errorf("tart is not installed. Please install it from https://github.com/cirruslabs/tart")
	}

	// Get current working directory
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("failed to get current directory: %w", err)
	}
	cwd, err = filepath.Abs(cwd)
	if err != nil {
		return fmt.Errorf("failed to get absolute path: %w", err)
	}

	// Extract directory name for dynamic mounting
	dirName := filepath.Base(cwd)

	// Create context with cancellation
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Handle interrupts
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigChan
		fmt.Fprintln(os.Stderr, "\nInterrupted, cleaning up...")
		cancel()
	}()

	// Create VM
	fmt.Fprintf(os.Stdout, "Creating ephemeral VM from %s...\n", vmImage)
	vm, err := tart.NewVMClonedFrom(ctx, vmImage, nil)
	if err != nil {
		return err
	}
	defer func() {
		fmt.Fprintln(os.Stdout, "Cleaning up VM...")
		if err := vm.Close(); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to clean up VM: %v\n", err)
		}
	}()

	// Configure VM
	fmt.Fprintln(os.Stdout, "Configuring VM...")
	if err := vm.Configure(ctx, cpuCount, memoryMB); err != nil {
		return err
	}

	// Start VM with directory mounts
	fmt.Fprintln(os.Stdout, "Starting VM...")
	directoryMounts := []tart.DirectoryMount{
		{
			Name:     dirName,
			Path:     cwd,
			ReadOnly: false,
		},
	}

	// Parse and add user-specified additional directories
	if len(extraDirs) > 0 {
		additionalMounts, err := parseDirectoryMounts(extraDirs)
		if err != nil {
			return err
		}
		// Prevent name collisions with the working directory mount
		for _, m := range additionalMounts {
			if m.Name == dirName {
				return fmt.Errorf("mount name %q conflicts with working directory name; please choose a different name", m.Name)
			}
		}
		directoryMounts = append(directoryMounts, additionalMounts...)
	}

	vm.Start(ctx, directoryMounts)

	// Wait for VM to get IP
	fmt.Fprintln(os.Stdout, "Waiting for VM to boot...")
	ip, err := vm.RetrieveIP(ctx)
	if err != nil {
		return fmt.Errorf("failed to get VM IP: %w", err)
	}
	fmt.Fprintf(os.Stdout, "VM IP: %s\n", ip)

	// Check for VM startup errors
	select {
	case err := <-vm.ErrChan():
		if err != nil {
			return fmt.Errorf("VM failed to start: %w", err)
		}
	default:
		// VM is running
	}

	// Connect via SSH
	fmt.Fprintln(os.Stdout, "Connecting to VM via SSH...")
	sshAddr := fmt.Sprintf("%s:22", ip)
	sshClient, err := ssh.WaitForSSH(ctx, sshAddr, sshUser, sshPass)
	if err != nil {
		return fmt.Errorf("failed to connect via SSH: %w", err)
	}
	defer sshClient.Close()

	// Create executor
	exec := executor.New(sshClient, cwd, dirName)

	// Mount working directory
	fmt.Fprintln(os.Stdout, "Mounting working directory...")
	if err := exec.MountWorkingDirectory(ctx); err != nil {
		return err
	}
	defer func() {
		_ = exec.UnmountWorkingDirectory(ctx)
	}()

	// Create symlinks for mounts with custom VM paths
	var symlinkMounts []executor.SymlinkMount
	for _, mount := range directoryMounts {
		if mount.VMPath != "" {
			symlinkMounts = append(symlinkMounts, executor.SymlinkMount{
				MountName: mount.Name,
				VMPath:    mount.VMPath,
				CopyMode:  mount.ReadOnly, // Copy read-only mounts to make them writable in VM
				HostPath:  mount.Path,     // Pass host path for user compatibility symlinks
			})
		}
	}
	if len(symlinkMounts) > 0 {
		fmt.Fprintln(os.Stdout, "Setting up custom mount paths...")
		if err := exec.CreateSymlinks(ctx, symlinkMounts); err != nil {
			return err
		}
		defer func() {
			_ = exec.CleanupSymlinks(ctx)
		}()
	}

	// Configure shared hostname if specified
	if sharedHostname != "" {
		fmt.Fprintf(os.Stdout, "Configuring shared hostname '%s'...\n", sharedHostname)
		hostIP, err := exec.ConfigureSharedHostname(ctx, sharedHostname)
		if err != nil {
			return fmt.Errorf("failed to configure shared hostname: %w", err)
		}

		// Print instructions for host configuration
		fmt.Fprintln(os.Stdout, strings.Repeat("-", 80))
		fmt.Fprintf(os.Stdout, "✓ VM configured: %s resolves to %s (host machine)\n", sharedHostname, hostIP)
		fmt.Fprintf(os.Stdout, "\nTo complete setup, add this to your host's /etc/hosts:\n")
		fmt.Fprintf(os.Stdout, "  127.0.0.1 %s\n", sharedHostname)
		fmt.Fprintf(os.Stdout, "\nRun: echo '127.0.0.1 %s' | sudo tee -a /etc/hosts\n", sharedHostname)
		fmt.Fprintln(os.Stdout, strings.Repeat("-", 80))
	}

	// Update Claude CLI to latest version (for claude command only)
	if len(args) > 0 && args[0] == "claude" {
		fmt.Fprintln(os.Stdout, "Updating Claude CLI to latest version...")
		if err := exec.UpdateClaude(ctx); err != nil {
			// Log warning but continue - don't fail if update has issues
			fmt.Fprintf(os.Stderr, "Warning: %v\n", err)
		}
	}

	// Execute command
	fmt.Fprintf(os.Stdout, "Executing command: %s %v\n", args[0], args[1:])
	fmt.Fprintln(os.Stdout, strings.Repeat("-", 80))

	// Use interactive or non-interactive execution based on the parameter
	if interactive {
		if err := exec.ExecuteInteractive(ctx, args[0], args[1:]); err != nil {
			return err
		}
	} else {
		if err := exec.Execute(ctx, args[0], args[1:]); err != nil {
			return err
		}
	}

	return nil
}
