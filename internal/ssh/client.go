package ssh

import (
	"context"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"github.com/avast/retry-go/v4"
	"golang.org/x/crypto/ssh"
)

// PortForwarder manages SSH local port forwarding
type PortForwarder struct {
	client     *ssh.Client
	localPort  int
	remotePort int
	listener   net.Listener
	wg         sync.WaitGroup
	ctx        context.Context
	cancel     context.CancelFunc
}

// NewPortForwarder creates a new SSH port forwarder.
// It forwards localPort on the host to remotePort on the VM's localhost.
func NewPortForwarder(client *ssh.Client, localPort, remotePort int) *PortForwarder {
	ctx, cancel := context.WithCancel(context.Background())
	return &PortForwarder{
		client:     client,
		localPort:  localPort,
		remotePort: remotePort,
		ctx:        ctx,
		cancel:     cancel,
	}
}

// Start begins listening on the local port and forwarding connections.
// Returns the actual local port being used (useful if localPort was 0).
func (pf *PortForwarder) Start() (int, error) {
	listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", pf.localPort))
	if err != nil {
		return 0, fmt.Errorf("failed to listen on port %d: %w", pf.localPort, err)
	}
	pf.listener = listener

	// Get actual port if we requested 0
	actualPort := listener.Addr().(*net.TCPAddr).Port

	pf.wg.Add(1)
	go pf.acceptLoop()

	return actualPort, nil
}

// Stop closes the listener and waits for all connections to finish
func (pf *PortForwarder) Stop() {
	pf.cancel()
	if pf.listener != nil {
		pf.listener.Close()
	}
	pf.wg.Wait()
}

func (pf *PortForwarder) acceptLoop() {
	defer pf.wg.Done()

	for {
		conn, err := pf.listener.Accept()
		if err != nil {
			select {
			case <-pf.ctx.Done():
				return
			default:
				// Listener closed
				return
			}
		}

		pf.wg.Add(1)
		go pf.handleConnection(conn)
	}
}

func (pf *PortForwarder) handleConnection(localConn net.Conn) {
	defer pf.wg.Done()
	defer localConn.Close()

	// Connect to remote port through SSH tunnel
	remoteAddr := fmt.Sprintf("127.0.0.1:%d", pf.remotePort)
	remoteConn, err := pf.client.Dial("tcp", remoteAddr)
	if err != nil {
		return
	}
	defer remoteConn.Close()

	// Bidirectional copy
	done := make(chan struct{}, 2)

	go func() {
		io.Copy(remoteConn, localConn)
		done <- struct{}{}
	}()

	go func() {
		io.Copy(localConn, remoteConn)
		done <- struct{}{}
	}()

	// Wait for either direction to finish or context cancellation
	select {
	case <-done:
	case <-pf.ctx.Done():
	}
}

func WaitForSSH(
	ctx context.Context,
	addr string,
	sshUser string,
	sshPassword string,
) (*ssh.Client, error) {
	var sshConn ssh.Conn
	var chans <-chan ssh.NewChannel
	var reqs <-chan *ssh.Request

	if err := retry.Do(func() error {
		var netConn net.Conn
		var err error

		boundedCtx, cancel := context.WithTimeout(ctx, time.Second)
		defer cancel()

		dialer := net.Dialer{}
		netConn, err = dialer.DialContext(boundedCtx, "tcp", addr)
		if err != nil {
			return err
		}

		sshConfig := &ssh.ClientConfig{
			HostKeyCallback: ssh.InsecureIgnoreHostKey(),
			User:            sshUser,
			Auth: []ssh.AuthMethod{
				ssh.Password(sshPassword),
			},
			Timeout: time.Second,
		}

		sshConn, chans, reqs, err = ssh.NewClientConn(netConn, addr, sshConfig)
		if err != nil {
			return fmt.Errorf("failed to connect via SSH: %w", err)
		}

		return nil
	}, retry.Context(ctx),
		retry.Attempts(0),
		retry.DelayType(retry.FixedDelay),
		retry.Delay(time.Second),
	); err != nil {
		return nil, fmt.Errorf("failed to connect via SSH: %w", err)
	}

	return ssh.NewClient(sshConn, chans, reqs), nil
}
