// Package userservice manages native per-user service definitions, not processes.
package userservice

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/samsaffron/term-llm/internal/config"
)

const Version = 1
const Marker = "Managed by term-llm service; edit with service install"

type Spec struct {
	Version     int               `json:"version"`
	Kind        string            `json:"kind"`
	Binary      string            `json:"binary"`
	Directory   string            `json:"working_directory"`
	Args        []string          `json:"args"`
	Environment map[string]string `json:"environment"`
	Secrets     []string          `json:"secrets,omitempty"`
	Auth        string            `json:"auth"`
	URL         string            `json:"url"`
	Host        string            `json:"host"`
	Port        int               `json:"port"`
	BasePath    string            `json:"base_path"`
	AuthFile    string            `json:"auth_file,omitempty"`
	Register    bool              `json:"register,omitempty"`
}

func ValidKind(kind string) bool { return kind == "web" || kind == "hub" }
func (s Spec) Validate() error {
	if s.Version != Version || !ValidKind(s.Kind) {
		return fmt.Errorf("unsupported service specification")
	}
	if !filepath.IsAbs(s.Binary) || !filepath.IsAbs(s.Directory) {
		return fmt.Errorf("service executable and working directory must be absolute")
	}
	if s.Auth != "passkey" && s.Auth != "bearer" && s.Auth != "none" {
		return fmt.Errorf("invalid service auth mode")
	}
	if s.Port < 1 || s.Port > 65535 {
		return fmt.Errorf("invalid service port")
	}
	for k, v := range s.Environment {
		if !EnvironmentKey(k) || strings.ContainsRune(v, 0) {
			return fmt.Errorf("invalid service environment entry %q", k)
		}
	}
	for _, name := range s.Secrets {
		if !SecretName(name) {
			return fmt.Errorf("invalid service credential name %q", name)
		}
	}
	for _, v := range append(append([]string{s.Binary, s.Directory, s.URL}, s.Args...), s.AuthFile) {
		if strings.ContainsAny(v, "\x00\r\n") {
			return fmt.Errorf("service values must not contain control lines")
		}
	}
	return nil
}

func EnvironmentKey(k string) bool {
	switch k {
	case "HOME", "PATH", "SHELL", "LANG", "LC_ALL", "LC_CTYPE", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_CACHE_HOME":
		return true
	}
	return false
}

func ReadPrivate(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return nil, fmt.Errorf("%s must be a private regular file (0600)", path)
	}
	if info.Size() > 1<<20 {
		return nil, fmt.Errorf("service file is too large")
	}
	return os.ReadFile(path)
}

func PrivateDir(dir string) error {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0077 != 0 {
		return fmt.Errorf("service directory %s must be private (0700) and not a symlink", dir)
	}
	return nil
}

func WritePrivate(path string, data []byte) error {
	if err := PrivateDir(filepath.Dir(path)); err != nil {
		return err
	}
	if _, err := os.Lstat(path); err == nil {
		if _, err = ReadPrivate(path); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	return config.WriteFileAtomicallyNoFollow(path, data, 0600)
}

func Load(path string) (Spec, error) {
	var s Spec
	data, err := ReadPrivate(path)
	if err != nil {
		return s, err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err = dec.Decode(&s); err != nil {
		return s, fmt.Errorf("read service specification: %w", err)
	}
	if dec.Decode(&struct{}{}) != io.EOF {
		return s, fmt.Errorf("trailing service specification data")
	}
	return s, s.Validate()
}
func Save(path string, s Spec) error {
	if err := s.Validate(); err != nil {
		return err
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return WritePrivate(path, append(data, '\n'))
}

// RunnerEnvironment avoids inheriting an unrelated shell/manager's credentials
// or loader hooks. Dynamic desktop/session endpoints come from the supervisor.
func RunnerEnvironment(s Spec, inherited []string, secrets map[string]string) []string {
	env := map[string]string{}
	for _, entry := range inherited {
		k, v, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}
		switch k {
		case "XDG_RUNTIME_DIR", "DBUS_SESSION_BUS_ADDRESS", "DISPLAY", "WAYLAND_DISPLAY", "SSH_AUTH_SOCK", "TMPDIR":
			env[k] = v
		}
	}
	for k, v := range s.Environment {
		env[k] = v
	}
	for k, v := range secrets {
		env[k] = v
	}
	result := make([]string, 0, len(env))
	for k, v := range env {
		result = append(result, k+"="+v)
	}
	return result
}
