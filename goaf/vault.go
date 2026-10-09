package main

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/term"
)

// vaultHeader marks vault-encrypted values in playbook vars.
const vaultHeader = "$GOAFVAULT;1.1;AES256"

// vaultKey derives a 32-byte key from the password.
func vaultKey(password string) []byte {
	sum := sha256.Sum256([]byte(password))
	return sum[:]
}

// encryptVault encrypts plaintext with the password, returning the
// portable $GOAFVAULT;... envelope.
func encryptVault(plaintext, password string) (string, error) {
	if password == "" {
		return "", fmt.Errorf("vault password is empty")
	}
	block, err := aes.NewCipher(vaultKey(password))
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	ct := gcm.Seal(nonce, nonce, []byte(plaintext), nil)
	return vaultHeader + "\n" + base64.StdEncoding.EncodeToString(ct), nil
}

// decryptVault reverses encryptVault.
func decryptVault(envelope, password string) (string, error) {
	if password == "" {
		return "", fmt.Errorf("vault-encrypted value requires a password (--vault-pass-file, --ask-vault-pass or GOAF_VAULT_PASSWORD)")
	}
	body := strings.TrimPrefix(envelope, vaultHeader)
	body = strings.TrimSpace(body)
	raw, err := base64.StdEncoding.DecodeString(body)
	if err != nil {
		return "", fmt.Errorf("bad vault envelope: %w", err)
	}
	block, err := aes.NewCipher(vaultKey(password))
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	if len(raw) < gcm.NonceSize() {
		return "", fmt.Errorf("bad vault envelope: too short")
	}
	nonce, ct := raw[:gcm.NonceSize()], raw[gcm.NonceSize():]
	pt, err := gcm.Open(nil, nonce, ct, nil)
	if err != nil {
		return "", fmt.Errorf("vault decrypt failed (wrong password?): %w", err)
	}
	return string(pt), nil
}

// vaultPwCache holds a prompted password so the user is asked only once.
var vaultPwCache string

// vaultPassword resolves the vault password from --vault-pass-file,
// GOAF_VAULT_PASSWORD, or an interactive prompt (when allowPrompt).
func vaultPassword(passFile string, allowPrompt bool) (string, error) {
	if vaultPwCache != "" {
		return vaultPwCache, nil
	}
	if passFile != "" {
		data, err := os.ReadFile(passFile)
		if err != nil {
			return "", fmt.Errorf("reading vault pass file: %w", err)
		}
		if pw := strings.TrimSpace(string(data)); pw != "" {
			return pw, nil
		}
	}
	if pw := os.Getenv("GOAF_VAULT_PASSWORD"); pw != "" {
		return pw, nil
	}
	if allowPrompt {
		pw, err := readPassword("VAULT password: ")
		if err != nil {
			return "", err
		}
		vaultPwCache = pw
		return pw, nil
	}
	return "", fmt.Errorf("no vault password (--vault-pass-file, --ask-vault-pass or GOAF_VAULT_PASSWORD)")
}

// decryptVaultValue decrypts $GOAFVAULT values when a password is available.
func decryptVaultValue(v, passFile string, allowPrompt bool) (string, error) {
	if !strings.HasPrefix(v, vaultHeader) {
		return v, nil
	}
	pw, err := vaultPassword(passFile, allowPrompt)
	if err != nil {
		return "", err
	}
	return decryptVault(v, pw)
}

// readPassword prompts on the terminal without echoing input.
func readPassword(prompt string) (string, error) {
	fmt.Fprint(os.Stderr, prompt)
	pw, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return "", err
	}
	return string(pw), nil
}
