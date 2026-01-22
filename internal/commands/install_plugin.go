package commands

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/cirruslabs/chamber/internal/ssh"
	"github.com/cirruslabs/chamber/internal/vm/tart"
	"github.com/spf13/cobra"
)

func NewInstallPluginCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "install-plugin <plugin-name>",
		Short: "Install a Claude plugin into the chamber-seed VM",
		Long: `Install a Claude plugin into the chamber-seed VM so it's available in all future ephemeral VMs.

The plugin will be installed globally via npm in the seed VM.

Example:
  chamber install-plugin @anthropic-ai/basic-memory
  chamber install-plugin my-custom-plugin`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			pluginName := args[0]
			return runInstallPlugin(cmd.Context(), pluginName)
		},
	}

	return cmd
}

func runInstallPlugin(ctx context.Context, pluginName string) error {
	// Check if Tart is installed
	if !tart.Installed() {
		return fmt.Errorf("tart is not installed. Please install it from https://github.com/cirruslabs/tart")
	}

	// Check if chamber-seed exists
	if !tart.VMExists("chamber-seed") {
		return fmt.Errorf("chamber-seed VM does not exist. Please run 'chamber init <remote-vm>' first")
	}

	// Create context with cancellation if not provided
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

	// Start the chamber-seed VM
	fmt.Fprintln(os.Stdout, "Starting chamber-seed VM...")
	vm, err := tart.NewVM(ctx, "chamber-seed")
	if err != nil {
		return fmt.Errorf("failed to create VM: %w", err)
	}
	defer func() {
		fmt.Fprintln(os.Stdout, "Stopping VM...")
		if err := vm.StopWithContext(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to stop VM: %v\n", err)
		}
	}()

	// Start VM without directory mounts
	vm.Start(ctx, nil)

	// Wait for VM to get IP
	fmt.Fprintln(os.Stdout, "Waiting for VM to boot...")
	ip, err := vm.RetrieveIP(ctx)
	if err != nil {
		return fmt.Errorf("failed to get VM IP: %w", err)
	}
	fmt.Fprintf(os.Stdout, "VM IP: %s\n", ip)

	// Connect via SSH
	fmt.Fprintln(os.Stdout, "Connecting to VM via SSH...")
	sshAddr := fmt.Sprintf("%s:22", ip)
	sshClient, err := ssh.WaitForSSH(ctx, sshAddr, "admin", "admin")
	if err != nil {
		return fmt.Errorf("failed to connect via SSH: %w", err)
	}
	defer sshClient.Close()

	// Install the plugin
	fmt.Fprintf(os.Stdout, "Installing %s...\n", pluginName)
	session, err := sshClient.NewSession()
	if err != nil {
		return fmt.Errorf("failed to create SSH session: %w", err)
	}
	defer session.Close()

	session.Stdout = os.Stdout
	session.Stderr = os.Stderr
	installCmd := fmt.Sprintf("zsh -l -c 'npm install -g %s'", pluginName)
	if err := session.Run(installCmd); err != nil {
		return fmt.Errorf("failed to install plugin: %w", err)
	}

	fmt.Fprintf(os.Stdout, "\n✓ Plugin %s installed successfully in chamber-seed!\n", pluginName)
	fmt.Fprintln(os.Stdout, "The plugin will be available in all future ephemeral VMs.")
	return nil
}
