package main

import (
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
	"golang.org/x/crypto/ssh/knownhosts"
)

// Session represents a single SSH connection to one host.
type Session struct {
	Host       string // "addr" or "addr:port" for display
	Become     bool   // prefix commands with sudo when true
	client     *ssh.Client
	jumpClient *ssh.Client // non-nil when tunnelled through a jump host
}

// authMethods collects available authentication methods.
// Priority: 1) SSH agent, 2) keyPath if set, 3) ~/.ssh/id_ed25519 and ~/.ssh/id_rsa.
func authMethods(keyPath string) ([]ssh.AuthMethod, error) {
	var methods []ssh.AuthMethod

	if sock := os.Getenv("SSH_AUTH_SOCK"); sock != "" {
		if conn, err := net.Dial("unix", sock); err == nil {
			ag := agent.NewClient(conn)
			methods = append(methods, ssh.PublicKeysCallback(ag.Signers))
		}
	}

	if keyPath != "" {
		expanded := expandPath(keyPath)
		if data, err := os.ReadFile(expanded); err == nil {
			if signer, err := ssh.ParsePrivateKey(data); err == nil {
				methods = append(methods, ssh.PublicKeys(signer))
			}
		}
	} else {
		for _, name := range []string{"id_ed25519", "id_rsa"} {
			p := filepath.Join(os.Getenv("HOME"), ".ssh", name)
			if data, err := os.ReadFile(p); err == nil {
				if signer, err := ssh.ParsePrivateKey(data); err == nil {
					methods = append(methods, ssh.PublicKeys(signer))
				}
			}
		}
	}

	if len(methods) == 0 {
		return nil, fmt.Errorf("no SSH auth methods available (no agent, no ~/.ssh/id_ed25519, no ~/.ssh/id_rsa)")
	}
	return methods, nil
}

func expandPath(p string) string {
	if strings.HasPrefix(p, "~/") {
		return filepath.Join(os.Getenv("HOME"), p[2:])
	}
	return p
}

// Connect opens an SSH connection to the host with known_hosts verification.
// When h.JumpAddr is set, the connection is tunnelled through the jump host.
func Connect(h Host) (*Session, error) {
	methods, err := authMethods(h.Key)
	if err != nil {
		return nil, err
	}

	knownHostsPath := filepath.Join(os.Getenv("HOME"), ".ssh", "known_hosts")
	hostKeyCallback, err := knownhosts.New(knownHostsPath)
	if err != nil {
		return nil, fmt.Errorf(
			"loading known_hosts ('%s'): %w\nAdd host key with: ssh-keyscan -p %d %s >> ~/.ssh/known_hosts",
			knownHostsPath, err, h.Port, h.Addr,
		)
	}

	baseCfg := &ssh.ClientConfig{
		Auth:            methods,
		HostKeyCallback: hostKeyCallback,
		Timeout:         10 * time.Second,
	}

	label := h.Addr
	if h.Port != 22 {
		label = fmt.Sprintf("%s:%d", h.Addr, h.Port)
	}

	if h.JumpAddr != "" {
		return connectViaJump(h, baseCfg, label)
	}

	cfg := *baseCfg
	cfg.User = h.User
	addr := fmt.Sprintf("%s:%d", h.Addr, h.Port)
	client, err := ssh.Dial("tcp", addr, &cfg)
	if err != nil {
		return nil, err
	}
	return &Session{Host: label, client: client}, nil
}

func connectViaJump(h Host, baseCfg *ssh.ClientConfig, label string) (*Session, error) {
	jumpAddr := fmt.Sprintf("%s:%d", h.JumpAddr, h.JumpPort)
	jumpCfg := *baseCfg
	jumpCfg.User = h.JumpUser
	jumpClient, err := ssh.Dial("tcp", jumpAddr, &jumpCfg)
	if err != nil {
		return nil, fmt.Errorf("jump host %s: %w", jumpAddr, err)
	}

	targetAddr := fmt.Sprintf("%s:%d", h.Addr, h.Port)
	conn, err := jumpClient.Dial("tcp", targetAddr)
	if err != nil {
		jumpClient.Close()
		return nil, fmt.Errorf("tunnelling via %s to %s: %w", jumpAddr, targetAddr, err)
	}

	targetCfg := *baseCfg
	targetCfg.User = h.User
	ncc, chans, reqs, err := ssh.NewClientConn(conn, targetAddr, &targetCfg)
	if err != nil {
		conn.Close()
		jumpClient.Close()
		return nil, fmt.Errorf("SSH handshake (via jump) to %s: %w", targetAddr, err)
	}
	return &Session{Host: label, client: ssh.NewClient(ncc, chans, reqs), jumpClient: jumpClient}, nil
}

// Run executes a command and returns combined stdout+stderr.
// When s.Become is true, commands are prefixed with sudo unless they already start with sudo.
func (s *Session) Run(cmd string) (string, error) {
	if s.Become && !strings.HasPrefix(strings.TrimSpace(cmd), "sudo ") {
		cmd = "sudo " + cmd
	}
	sess, err := s.client.NewSession()
	if err != nil {
		return "", err
	}
	defer sess.Close()

	out, err := sess.CombinedOutput(cmd)
	return string(out), err
}

// Upload copies a local file to a remote path via SFTP.
func (s *Session) Upload(localPath, remotePath string) error {
	client, err := sftp.NewClient(s.client)
	if err != nil {
		return fmt.Errorf("SFTP client: %w", err)
	}
	defer client.Close()

	src, err := os.Open(localPath)
	if err != nil {
		return fmt.Errorf("opening local file: %w", err)
	}
	defer src.Close()

	if err := client.MkdirAll(filepath.Dir(remotePath)); err != nil {
		return fmt.Errorf("creating remote directory: %w", err)
	}

	dst, err := client.Create(remotePath)
	if err != nil {
		return fmt.Errorf("creating remote file: %w", err)
	}
	defer dst.Close()

	if _, err := io.Copy(dst, src); err != nil {
		return fmt.Errorf("transferring data: %w", err)
	}
	return nil
}

// UploadContent writes byte content directly to a remote path via SFTP.
func (s *Session) UploadContent(content []byte, remotePath string) error {
	client, err := sftp.NewClient(s.client)
	if err != nil {
		return fmt.Errorf("SFTP client: %w", err)
	}
	defer client.Close()

	if err := client.MkdirAll(filepath.Dir(remotePath)); err != nil {
		return fmt.Errorf("creating remote directory: %w", err)
	}

	dst, err := client.Create(remotePath)
	if err != nil {
		return fmt.Errorf("creating remote file: %w", err)
	}
	defer dst.Close()

	if _, err := dst.Write(content); err != nil {
		return fmt.Errorf("writing content: %w", err)
	}
	return nil
}

// ReadRemote reads the contents of a file on the remote host via SFTP.
func (s *Session) ReadRemote(remotePath string) ([]byte, error) {
	client, err := sftp.NewClient(s.client)
	if err != nil {
		return nil, fmt.Errorf("SFTP client: %w", err)
	}
	defer client.Close()

	f, err := client.Open(remotePath)
	if err != nil {
		return nil, fmt.Errorf("opening remote file: %w", err)
	}
	defer f.Close()

	return io.ReadAll(f)
}

func (s *Session) Close() {
	if s.client != nil {
		s.client.Close()
	}
	if s.jumpClient != nil {
		s.jumpClient.Close()
	}
}
