package executor

import (
	"bufio"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/cirruslabs/chamber/internal/ssh"
	gossh "golang.org/x/crypto/ssh"
)

// SymlinkMount represents a directory mount that needs a symlink
type SymlinkMount struct {
	MountName string
	VMPath    string
	CopyMode  bool   // If true, copy contents instead of symlinking (for read-only mounts)
	HostPath  string // Original host path, used to create user compatibility symlinks
}

type Executor struct {
	sshClient      *gossh.Client
	workingDir     string
	mountedWorkDir string
	dirName        string
	symlinks       []struct{ from, to string } // Track symlinks for cleanup
	envVars        map[string]string           // Environment variables to set in VM
}

func New(sshClient *gossh.Client, workingDir string, dirName string) *Executor {
	return &Executor{
		sshClient:      sshClient,
		workingDir:     workingDir,
		mountedWorkDir: fmt.Sprintf("$HOME/workspace/%s", dirName),
		dirName:        dirName,
		envVars:        make(map[string]string),
	}
}

// SetEnv sets an environment variable to be passed to commands executed in the VM
func (e *Executor) SetEnv(key, value string) {
	e.envVars[key] = value
}

// ForwardCredentials copies Claude credentials from the host to the VM.
// This allows Claude Max OAuth tokens to work in the VM without mounting ~/.claude read-write.
func (e *Executor) ForwardCredentials(ctx context.Context, credentialsJSON string) error {
	if credentialsJSON == "" {
		return nil
	}

	session, err := e.sshClient.NewSession()
	if err != nil {
		return fmt.Errorf("failed to create SSH session: %w", err)
	}
	defer session.Close()

	// Escape the JSON for shell - use base64 to avoid quoting issues
	encoded := base64.StdEncoding.EncodeToString([]byte(credentialsJSON))

	// Create ~/.claude directory and write credentials file
	// Uses base64 to safely transfer JSON content through shell
	command := fmt.Sprintf(`mkdir -p ~/.claude && echo '%s' | base64 -d > ~/.claude/.credentials.json && chmod 600 ~/.claude/.credentials.json`, encoded)

	if err := session.Run(command); err != nil {
		return fmt.Errorf("failed to write credentials: %w", err)
	}

	return nil
}

func (e *Executor) MountWorkingDirectory(ctx context.Context) error {
	session, err := e.sshClient.NewSession()
	if err != nil {
		return fmt.Errorf("failed to create SSH session: %w", err)
	}
	defer session.Close()

	// Unmount any existing shared files and create workspace directory
	// Then mount virtiofs with the automount tag
	commands := []string{
		`sudo umount "/Volumes/My Shared Files"`,
		`mkdir -p ~/workspace`,
		`mount_virtiofs com.apple.virtio-fs.automount ~/workspace`,
	}

	command := strings.Join(commands, " && ")

	if err := session.Run(command); err != nil {
		return fmt.Errorf("failed to mount working directory: %w", err)
	}

	return nil
}

func (e *Executor) UnmountWorkingDirectory(ctx context.Context) error {
	session, err := e.sshClient.NewSession()
	if err != nil {
		return fmt.Errorf("failed to create SSH session: %w", err)
	}
	defer session.Close()

	command := fmt.Sprintf("umount %q", e.mountedWorkDir)

	// Ignore errors on unmount as it might have been unmounted already
	_ = session.Run(command)

	return nil
}

// CreateSymlinks creates symlinks for directory mounts with custom VM paths.
// mountName is the name used in ~/workspace/<mountName>
// vmPath is the desired location in the VM (e.g., ~/.claude)
func (e *Executor) CreateSymlinks(ctx context.Context, mounts []SymlinkMount) error {
	if len(mounts) == 0 {
		return nil
	}

	session, err := e.sshClient.NewSession()
	if err != nil {
		return fmt.Errorf("failed to create SSH session: %w", err)
	}
	defer session.Close()

	// Capture stdout and stderr for debugging
	var stdout, stderr strings.Builder
	session.Stdout = &stdout
	session.Stderr = &stderr

	var commands []string

	// Track host user directories that need compatibility symlinks
	// e.g., if host path is /Users/saint/.claude, we need /Users/saint -> /Users/admin
	hostUserDirs := make(map[string]bool)
	macUserPattern := regexp.MustCompile(`^/Users/([^/]+)`)

	for _, m := range mounts {
		sourcePath := fmt.Sprintf("$HOME/workspace/%s", m.MountName)
		targetPath := m.VMPath

		// Expand ~ to $HOME for consistency
		if targetPath == "~" {
			targetPath = "$HOME"
		} else if strings.HasPrefix(targetPath, "~/") {
			targetPath = "$HOME/" + targetPath[2:]
		}

		// Create parent directory if needed, remove existing file/link
		commands = append(commands,
			fmt.Sprintf("mkdir -p $(dirname %s)", targetPath),
			fmt.Sprintf("rm -rf %s", targetPath),
		)

		if m.CopyMode {
			// Copy the entire directory (for read-only mounts that need to be writable in VM)
			// Use rsync with flags to avoid extended attribute issues with symlinks
			// -r = recursive
			// -l = preserve symlinks
			// -t = preserve timestamps
			// -D = preserve devices and special files
			// (equivalent to -a but without -p/-o/-g which try to preserve permissions/ownership)
			commands = append(commands,
				fmt.Sprintf("rsync -rltD %s/ %s/", sourcePath, targetPath),
			)

			// Track host user directory for compatibility symlink
			if m.HostPath != "" {
				if matches := macUserPattern.FindStringSubmatch(m.HostPath); len(matches) > 1 {
					hostUserDirs[matches[1]] = true
				}
			}
		} else {
			// Create symlink (default behavior)
			commands = append(commands,
				fmt.Sprintf("ln -s %s %s", sourcePath, targetPath),
			)
		}

		e.symlinks = append(e.symlinks, struct{ from, to string }{from: targetPath, to: sourcePath})
	}

	// Create compatibility symlinks for host user directories
	// This ensures paths like /Users/saint/.claude/plugins/cache/... work in the VM
	// by creating /Users/saint -> /Users/<vm-user>
	for hostUser := range hostUserDirs {
		// Create symlink: /Users/<host-user> -> /Users/<vm-user>
		// We use $USER to get the VM's current username dynamically
		commands = append(commands,
			fmt.Sprintf(`if [ ! -d "/Users/%s" ] && [ "$USER" != "%s" ]; then sudo mkdir -p /Users && sudo ln -sf "/Users/$USER" "/Users/%s"; fi`,
				hostUser, hostUser, hostUser),
		)
		e.symlinks = append(e.symlinks, struct{ from, to string }{from: fmt.Sprintf("/Users/%s", hostUser), to: "/Users/$USER"})
	}

	command := strings.Join(commands, " && ")
	if err := session.Run(command); err != nil {
		errMsg := stderr.String()
		if errMsg != "" {
			return fmt.Errorf("failed to create symlinks: %w (stderr: %s)", err, errMsg)
		}
		return fmt.Errorf("failed to create symlinks: %w", err)
	}

	// Print debug output if any
	if out := stdout.String(); out != "" {
		fmt.Fprintln(os.Stdout, out)
	}

	return nil
}

// CleanupSymlinks removes symlinks created by CreateSymlinks
func (e *Executor) CleanupSymlinks(ctx context.Context) error {
	if len(e.symlinks) == 0 {
		return nil
	}

	session, err := e.sshClient.NewSession()
	if err != nil {
		return fmt.Errorf("failed to create SSH session: %w", err)
	}
	defer session.Close()

	var commands []string
	for _, link := range e.symlinks {
		commands = append(commands, fmt.Sprintf("rm -f %s", link.from))
	}

	command := strings.Join(commands, " ; ") // Use ; to continue even if some fail

	// Ignore errors on cleanup
	_ = session.Run(command)

	return nil
}

// DetectPlannotator checks if plannotator is installed in the VM
// Returns the configured port (from PLANNOTATOR_PORT env) or 19432 as default
func (e *Executor) DetectPlannotator(ctx context.Context) (bool, int, error) {
	session, err := e.sshClient.NewSession()
	if err != nil {
		return false, 0, fmt.Errorf("failed to create SSH session: %w", err)
	}
	defer session.Close()

	// Check if plannotator exists in common locations
	// Uses login shell to ensure PATH is loaded properly
	var stdout strings.Builder
	session.Stdout = &stdout

	// Check for plannotator and get PLANNOTATOR_PORT if set
	command := `zsh -l -c 'which plannotator >/dev/null 2>&1 && echo "found" && echo "${PLANNOTATOR_PORT:-19432}" || echo "notfound"'`
	if err := session.Run(command); err != nil {
		// Command failed - plannotator not found
		return false, 0, nil
	}

	output := strings.TrimSpace(stdout.String())
	lines := strings.Split(output, "\n")

	if len(lines) < 1 || lines[0] != "found" {
		return false, 0, nil
	}

	// Parse port
	port := 19432
	if len(lines) >= 2 {
		if p, err := strconv.Atoi(strings.TrimSpace(lines[1])); err == nil && p > 0 {
			port = p
		}
	}

	return true, port, nil
}

// UpdateClaude updates Claude CLI to the latest version in the VM
func (e *Executor) UpdateClaude(ctx context.Context) error {
	session, err := e.sshClient.NewSession()
	if err != nil {
		return fmt.Errorf("failed to create SSH session: %w", err)
	}
	defer session.Close()

	// Run npm install -g to update Claude CLI
	// Use login shell (-l) to load user's PATH where npm is configured
	// Redirect output to /dev/null to keep it quiet, but preserve exit code
	command := "zsh -l -c 'npm install -g @anthropic-ai/claude-code@latest >/dev/null 2>&1'"

	if err := session.Run(command); err != nil {
		// Don't fail hard on update errors - just log a warning
		// This ensures Chamber still works even if npm has issues
		return fmt.Errorf("warning: failed to update Claude CLI (continuing anyway): %w", err)
	}

	return nil
}

// ConfigureSharedHostname sets up a hostname that resolves to the host machine from within the VM.
// This allows plugins to use the same hostname whether running on the host or in the VM.
func (e *Executor) ConfigureSharedHostname(ctx context.Context, hostname string) (string, error) {
	session, err := e.sshClient.NewSession()
	if err != nil {
		return "", fmt.Errorf("failed to create SSH session: %w", err)
	}
	defer session.Close()

	// Get the default gateway IP, which is the host machine in Tart VMs
	var stdout strings.Builder
	session.Stdout = &stdout

	// Use netstat to find the default gateway (the host machine)
	command := "netstat -nr | grep default | grep -v 'link#' | awk '{ print $2 }' | head -1"
	if err := session.Run(command); err != nil {
		return "", fmt.Errorf("failed to get default gateway: %w", err)
	}

	hostIP := strings.TrimSpace(stdout.String())
	if hostIP == "" {
		return "", fmt.Errorf("could not determine host IP (no default gateway found)")
	}

	// Add hostname entry to /etc/hosts pointing to the host IP
	session2, err := e.sshClient.NewSession()
	if err != nil {
		return "", fmt.Errorf("failed to create SSH session: %w", err)
	}
	defer session2.Close()

	addHostCommand := fmt.Sprintf("echo '%s %s' | sudo tee -a /etc/hosts >/dev/null", hostIP, hostname)
	if err := session2.Run(addHostCommand); err != nil {
		return "", fmt.Errorf("failed to add hostname to /etc/hosts: %w", err)
	}

	return hostIP, nil
}

func (e *Executor) Execute(ctx context.Context, command string, args []string) error {
	session, err := e.sshClient.NewSession()
	if err != nil {
		return fmt.Errorf("failed to create SSH session: %w", err)
	}
	defer session.Close()

	// Set up pipes for stdout and stderr
	stdout, err := session.StdoutPipe()
	if err != nil {
		return fmt.Errorf("failed to create stdout pipe: %w", err)
	}

	stderr, err := session.StderrPipe()
	if err != nil {
		return fmt.Errorf("failed to create stderr pipe: %w", err)
	}

	stdin, err := session.StdinPipe()
	if err != nil {
		return fmt.Errorf("failed to create stdin pipe: %w", err)
	}

	// Start output readers
	go e.streamOutput(stdout, os.Stdout)
	go e.streamOutput(stderr, os.Stderr)

	// Start a shell
	if err := session.Shell(); err != nil {
		return fmt.Errorf("failed to start shell: %w", err)
	}

	// Change to mounted working directory
	_, err = stdin.Write([]byte(fmt.Sprintf("cd %s\n", e.mountedWorkDir)))
	if err != nil {
		return fmt.Errorf("failed to change directory: %w", err)
	}

	// Execute the command
	fullCommand := fmt.Sprintf("%s %s", command, strings.Join(args, " "))
	_, err = stdin.Write([]byte(fullCommand + "\nexit $?\n"))
	if err != nil {
		return fmt.Errorf("failed to execute command: %w", err)
	}

	// Handle context cancellation
	go func() {
		<-ctx.Done()
		// Send interrupt signal to the shell
		_ = session.Signal(gossh.SIGINT)
		// Close the session to force termination
		_ = session.Close()
	}()

	// Wait for command to complete
	if err := session.Wait(); err != nil {
		// Check if context was cancelled
		if ctx.Err() != nil {
			return fmt.Errorf("command interrupted")
		}
		// Check if it's an exit error, which means the command ran but returned non-zero
		if exitErr, ok := err.(*gossh.ExitError); ok {
			// Return a more descriptive error
			return fmt.Errorf("command exited with status %d", exitErr.ExitStatus())
		}
		return fmt.Errorf("failed to run command: %w", err)
	}

	return nil
}

// ExecuteInteractive executes a command with full terminal proxying
func (e *Executor) ExecuteInteractive(ctx context.Context, command string, args []string) error {
	// Create terminal proxy
	terminal := ssh.NewTerminal(e.sshClient)

	// Build environment variable exports
	var envExports []string
	for key, value := range e.envVars {
		// Escape single quotes in value for shell safety
		escapedValue := strings.ReplaceAll(value, "'", "'\"'\"'")
		envExports = append(envExports, fmt.Sprintf("export %s='%s'", key, escapedValue))
	}

	// Build the full command with working directory change and login shell
	// Use zsh -l -c to ensure the user's profile is loaded (similar to init.go)
	var innerCommand string
	if len(envExports) > 0 {
		// Prepend environment exports before the actual command
		innerCommand = fmt.Sprintf("%s && cd %s && %s %s",
			strings.Join(envExports, " && "),
			e.mountedWorkDir, command, strings.Join(args, " "))
	} else {
		innerCommand = fmt.Sprintf("cd %s && %s %s", e.mountedWorkDir, command, strings.Join(args, " "))
	}
	fullCommand := fmt.Sprintf("zsh -l -c %q", innerCommand)

	// Execute with full terminal proxying
	return terminal.RunInteractiveCommand(ctx, fullCommand)
}

func (e *Executor) streamOutput(reader io.Reader, writer io.Writer) {
	scanner := bufio.NewScanner(reader)
	for scanner.Scan() {
		fmt.Fprintln(writer, scanner.Text())
	}
}
