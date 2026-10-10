package main

import (
	"fmt"
	"io"
	"net"
	"os"
	"path"
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
	Become     bool   // run commands with sudo when true
	client     *ssh.Client
	jumpClient *ssh.Client // non-nil when tunnelled through a jump host
	addr       string      // connection parameters, kept for waitForSSH
	port       int
	user       string
	keyPath    string
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
	return &Session{Host: label, client: client, addr: h.Addr, port: h.Port, user: h.User, keyPath: h.Key}, nil
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

// shQuote quotes a string for POSIX sh with single quotes.
func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// becomePassword, when non-empty, is used for sudo authentication
// (--ask-become-pass or GOAF_BECOME_PASSWORD). The password is piped to
// `sudo -S` on every command, so no timestamp caching across connections
// is needed; embedded hardcoded `sudo` calls (package modules) run as root
// and need no further authentication.
var becomePassword string

// Run executes a command and returns combined stdout+stderr.
// When s.Become is true, the whole command runs privileged via sudo,
// so compound commands (a && b) are fully covered, not just the first segment.
// Without a become password, -n fails fast instead of hanging on a prompt:
// NOPASSWD sudo is required in that case (see README).
func (s *Session) Run(cmd string) (string, error) {
	sess, err := s.client.NewSession()
	if err != nil {
		return "", err
	}
	defer sess.Close()

	if s.Become {
		if becomePassword != "" {
			sess.Stdin = strings.NewReader(becomePassword + "\n")
			cmd = "sudo -S -p '' sh -c " + shQuote(cmd)
		} else {
			cmd = "sudo -n sh -c " + shQuote(cmd)
		}
	}

	out, err := sess.CombinedOutput(cmd)
	if err != nil && becomePassword != "" && s.Become &&
		strings.Contains(string(out), "Sorry, try again") {
		return string(out), fmt.Errorf("sudo authentication failed (wrong become password?)")
	}
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

	// Remote paths always use forward slashes: path (not filepath),
	// otherwise filepath.Dir on Windows produces backslash paths.
	if err := client.MkdirAll(path.Dir(remotePath)); err != nil {
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

	// Remote paths always use forward slashes: path (not filepath),
	// otherwise filepath.Dir on Windows produces backslash paths.
	if err := client.MkdirAll(path.Dir(remotePath)); err != nil {
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

// waitForSSH polls until SSH on the session's host accepts connections again
// (used after reboot), or the timeout in seconds expires.
// Host key verification is skipped: keys may legitimately change after a
// reinstall, and this is only a reachability probe, not a session.
func waitForSSH(s *Session, timeoutSec int) error {
	if timeoutSec <= 0 {
		timeoutSec = 300
	}
	deadline := time.Now().Add(time.Duration(timeoutSec) * time.Second)
	addr := fmt.Sprintf("%s:%d", s.addr, s.port)
	for time.Now().Before(deadline) {
		if methods, err := authMethods(s.keyPath); err == nil {
			cfg := &ssh.ClientConfig{
				User:            s.user,
				Auth:            methods,
				HostKeyCallback: ssh.InsecureIgnoreHostKey(),
				Timeout:         5 * time.Second,
			}
			if c, derr := ssh.Dial("tcp", addr, cfg); derr == nil {
				c.Close()
				return nil
			}
		}
		time.Sleep(5 * time.Second)
	}
	return fmt.Errorf("SSH did not return within %ds on %s", timeoutSec, addr)
}
