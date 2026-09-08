package userservice

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

var secretIdentifier = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)

func SecretName(name string) bool {
	if !secretIdentifier.MatchString(name) || strings.HasPrefix(name, "LD_") || strings.HasPrefix(name, "DYLD_") {
		return false
	}
	// These are private, temporary enrollment capabilities owned by the runner.
	if strings.Contains(name, "BOOTSTRAP_TOKEN") || strings.Contains(name, "RECOVERY_TOKEN") {
		return false
	}
	for _, suffix := range []string{"_API_KEY", "_TOKEN", "_SECRET", "_PASSWORD", "_ACCESS_KEY", "_ACCESS_KEY_ID"} {
		if strings.HasSuffix(name, suffix) {
			return true
		}
	}
	return false
}

type Credentials struct{ OS, Dir, Kind string }

func (c Credentials) keychainService() string {
	sum := sha256.Sum256([]byte(c.Dir))
	return fmt.Sprintf("term-llm.service.%s.%x", c.Kind, sum[:8])
}

func (c Credentials) Put(ctx context.Context, name, value string) error {
	if !ValidKind(c.Kind) || !SecretName(name) || value == "" || strings.ContainsRune(value, 0) {
		return fmt.Errorf("invalid service credential %q", name)
	}
	if c.OS != "darwin" {
		return WritePrivate(filepath.Join(c.Dir, "credentials", name), []byte(value))
	}
	// security -i reads commands from stdin. Encode the value into a restricted
	// alphabet rather than interpreting arbitrary credentials as command syntax.
	// Apple SecurityTool/macOS/security.c uses a 4096-byte line buffer.
	line, err := c.keychainInput(name, value)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/usr/bin/security", "-i", "-q")
	cmd.Stdin = strings.NewReader(line)
	// Never attach stderr: OS tool diagnostics must not expose the input command.
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("could not save %s in login Keychain; unlock it and approve access: %w", name, err)
	}
	got, err := c.Get(ctx, name)
	if err != nil {
		return err
	}
	if got != value {
		return fmt.Errorf("Keychain verification failed for %s", name)
	}
	return nil
}
func (c Credentials) Get(ctx context.Context, name string) (string, error) {
	if !ValidKind(c.Kind) || !SecretName(name) {
		return "", fmt.Errorf("invalid service credential name")
	}
	if c.OS != "darwin" {
		data, err := ReadPrivate(filepath.Join(c.Dir, "credentials", name))
		return string(data), err
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/usr/bin/security", "find-generic-password", "-s", c.keychainService(), "-a", name, "-w")
	data, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("could not read %s from login Keychain; unlock it and approve access: %w", name, err)
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(data)))
	if err != nil {
		return "", fmt.Errorf("invalid managed Keychain credential %s", name)
	}
	return string(decoded), nil
}

// ImportSecrets is deliberately not a shell/dotenv evaluator: values are literal,
// lines are KEY=value, comments begin with #, and duplicate keys are errors.
func ImportSecrets(path string) (map[string]string, error) {
	data, err := ReadPrivate(path)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for i, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSuffix(line, "\r")
		if strings.TrimSpace(line) == "" || strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok || !SecretName(key) || value == "" || strings.ContainsRune(value, 0) {
			return nil, fmt.Errorf("invalid credential on line %d (use literal KEY=value; no shell syntax)", i+1)
		}
		if _, exists := out[key]; exists {
			return nil, fmt.Errorf("duplicate credential %s", key)
		}
		out[key] = value
	}
	return out, nil
}

func (c Credentials) Load(ctx context.Context, names []string) (map[string]string, error) {
	out := map[string]string{}
	for _, name := range names {
		value, err := c.Get(ctx, name)
		if err != nil {
			return nil, err
		}
		out[name] = value
	}
	return out, nil
}

func (c Credentials) keychainInput(name, value string) (string, error) {
	line := "add-generic-password -U -s " + c.keychainService() + " -a " + name + " -w " + base64.StdEncoding.EncodeToString([]byte(value)) + "\n"
	if len(line) >= 4096 {
		return "", fmt.Errorf("credential %s exceeds Keychain CLI input limit", name)
	}
	return line, nil
}
