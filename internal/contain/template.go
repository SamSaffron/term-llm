package contain

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"

	"github.com/samsaffron/term-llm/internal/config"
	"golang.org/x/term"
)

// claudeOAuthTokenPattern matches Claude Code long-lived OAuth tokens
// (e.g. sk-ant-oat01-...). Used to extract a token from the captured
// stdout of `claude setup-token`.
var claudeOAuthTokenPattern = regexp.MustCompile(`sk-ant-[A-Za-z0-9_\-]{20,}`)

// claudeSetupTokenRunner is overridable in tests; it runs `claude setup-token`
// with the given stdin and tees stdout/stderr through w. The captured stdout
// is returned for token parsing.
var claudeSetupTokenRunner = defaultClaudeSetupTokenRunner

const defaultWorkspacePath = "/workspace"

var placeholderPattern = regexp.MustCompile(`\{\{\s*([A-Za-z0-9_]+)\s*\}\}`)
var unresolvedPlaceholderPattern = regexp.MustCompile(`\{\{[^{}]+\}\}`)

type CreateOptions struct {
	Template string
	CWD      string
	Values   map[string]string
	NoInput  bool
	Stdin    io.Reader
	Stdout   io.Writer
}

type PlaceholderValues map[string]string

// NewPlaceholderValues builds the supported replacement values for contain
// templates.
func NewPlaceholderValues(name, cwd, configDir, containersDir, composePath string) PlaceholderValues {
	home, _ := os.UserHomeDir()
	agentImageDir := ""
	agentImageDockerfile := ""
	if dir, err := ImageDir("agent"); err == nil {
		agentImageDir = dir
		agentImageDockerfile = filepath.Join(dir, "Dockerfile")
	}
	agentImageHash, _ := AgentImageHash()
	return PlaceholderValues{
		"name":                   name,
		"project_name":           ProjectName(name),
		"config_dir":             configDir,
		"compose_path":           composePath,
		"containers_dir":         containersDir,
		"cwd":                    cwd,
		"home":                   home,
		"workspace":              defaultWorkspacePath,
		"agent_image_dir":        agentImageDir,
		"agent_image_dockerfile": agentImageDockerfile,
		"agent_image_hash":       agentImageHash,
		"AGENT_NAME":             name,
	}
}

// RenderTemplateContent applies simple placeholder substitution to data.
func RenderTemplateContent(data []byte, values PlaceholderValues) ([]byte, error) {
	var unknown []string
	seenUnknown := map[string]bool{}
	out := placeholderPattern.ReplaceAllFunc(data, func(match []byte) []byte {
		sub := placeholderPattern.FindSubmatch(match)
		key := strings.TrimSpace(string(sub[1]))
		value, ok := values[key]
		if !ok {
			if !seenUnknown[key] {
				unknown = append(unknown, key)
				seenUnknown[key] = true
			}
			return match
		}
		return []byte(value)
	})
	if len(unknown) > 0 {
		return nil, fmt.Errorf("unknown template placeholder(s): %s", strings.Join(unknown, ", "))
	}
	if unresolved := unresolvedPlaceholderPattern.Find(out); unresolved != nil {
		return nil, fmt.Errorf("unknown template placeholder %q", string(unresolved))
	}
	return out, nil
}

// CreateWorkspace renders the selected template into the named global contain
// workspace directory. Existing targets are never overwritten.
func CreateWorkspace(name string, opts CreateOptions) (string, error) {
	if err := ValidateName(name); err != nil {
		return "", err
	}
	cwd := opts.CWD
	if cwd == "" {
		var err error
		cwd, err = os.Getwd()
		if err != nil {
			return "", err
		}
	}
	if abs, err := filepath.Abs(cwd); err == nil {
		cwd = abs
	}

	containersDir, err := ContainersRoot()
	if err != nil {
		return "", err
	}
	targetDir, err := ContainerDir(name)
	if err != nil {
		return "", err
	}
	composePath, err := ComposePath(name)
	if err != nil {
		return "", err
	}

	if _, err := os.Stat(targetDir); err == nil {
		return "", fmt.Errorf("contain workspace %q already exists at %s", name, targetDir)
	} else if !os.IsNotExist(err) {
		return "", err
	}

	tmpl, err := LoadTemplate(opts.Template)
	if err != nil {
		return "", err
	}
	if tmpl.Builtin && tmpl.Name == "agent" {
		if _, err := SyncImage("agent", true); err != nil {
			return "", fmt.Errorf("sync agent image: %w", err)
		}
	}
	promptValues, err := ResolveTemplateValues(tmpl.Descriptor, opts.Values, opts.NoInput, opts.Stdin, opts.Stdout)
	if err != nil {
		return "", err
	}
	if tmpl.Builtin && tmpl.Name == "agent" {
		if err := addAgentTemplateValues(promptValues, opts); err != nil {
			return "", err
		}
	}
	values := NewPlaceholderValues(name, cwd, targetDir, containersDir, composePath)
	for k, v := range promptValues {
		values[k] = v
	}

	type renderedFile struct {
		Path string
		Data []byte
		Mode os.FileMode
	}
	rendered := make([]renderedFile, 0, len(tmpl.Files))
	for _, f := range tmpl.Files {
		targetRel, err := safeTemplateTargetPath(f.Path)
		if err != nil {
			return "", err
		}
		data, err := RenderTemplateContent(f.Data, values)
		if err != nil {
			return "", fmt.Errorf("render %s: %w", f.Path, err)
		}
		mode := f.Mode.Perm()
		if mode == 0 {
			mode = 0o644
		}
		if filepath.Base(targetRel) == ".env" {
			mode = 0o600
		}
		if mode&0o111 != 0 {
			mode |= 0o111
		}
		rendered = append(rendered, renderedFile{Path: targetRel, Data: data, Mode: mode})
	}

	if err := os.MkdirAll(containersDir, 0o755); err != nil {
		return "", err
	}
	if err := os.Mkdir(targetDir, 0o755); err != nil {
		if os.IsExist(err) {
			return "", fmt.Errorf("contain workspace %q already exists at %s", name, targetDir)
		}
		return "", err
	}

	for _, f := range rendered {
		path := filepath.Join(targetDir, f.Path)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return "", err
		}
		if err := writeNewFile(path, f.Data, f.Mode); err != nil {
			return "", err
		}
	}
	return targetDir, nil
}

func addAgentTemplateValues(values map[string]string, opts CreateOptions) error {
	if values == nil {
		return nil
	}
	if err := addAgentDistroTemplateValues(values); err != nil {
		return err
	}
	if err := addChatGPTOAuthTemplateValue(values); err != nil {
		return err
	}
	if err := addClaudeCodeOAuthTemplateValue(values, opts); err != nil {
		return err
	}
	return nil
}

func addAgentDistroTemplateValues(values map[string]string) error {
	distro := values["agent_distro"]
	if distro == "" {
		distro = defaultAgentDistro()
		values["agent_distro"] = distro
	}
	switch distro {
	case "arch":
		setDefaultTemplateValue(values, "agent_platform", "linux/amd64")
		setDefaultTemplateValue(values, "agent_base_image", "archlinux:latest")
	case "fedora":
		setDefaultTemplateValue(values, "agent_platform", nativeLinuxPlatform())
		setDefaultTemplateValue(values, "agent_base_image", "fedora:43")
	default:
		return fmt.Errorf("unsupported agent_distro %q: expected arch or fedora", distro)
	}
	return nil
}

func defaultAgentDistro() string {
	if runtime.GOOS == "darwin" && runtime.GOARCH == "arm64" {
		return "fedora"
	}
	return "arch"
}

func nativeLinuxPlatform() string {
	switch runtime.GOARCH {
	case "arm64":
		return "linux/arm64"
	case "amd64":
		return "linux/amd64"
	default:
		return "linux/amd64"
	}
}

func setDefaultTemplateValue(values map[string]string, key, value string) {
	if values[key] == "" {
		values[key] = value
	}
}

func addChatGPTOAuthTemplateValue(values map[string]string) error {
	if _, ok := values["chatgpt_oauth_json_b64"]; ok {
		return nil
	}
	values["chatgpt_oauth_json_b64"] = ""
	if values["provider"] != "chatgpt" {
		return nil
	}
	configDir, err := config.GetConfigDir()
	if err != nil {
		return err
	}
	data, err := os.ReadFile(filepath.Join(configDir, "chatgpt_oauth.json"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	values["chatgpt_oauth_json_b64"] = base64.StdEncoding.EncodeToString(data)
	return nil
}

func addClaudeCodeOAuthTemplateValue(values map[string]string, opts CreateOptions) error {
	if _, ok := values["claude_code_oauth_token"]; ok {
		return nil
	}
	values["claude_code_oauth_token"] = ""
	if values["provider"] != "claude-bin" {
		return nil
	}
	token, err := mintClaudeCodeOAuthToken(opts)
	if err != nil {
		return err
	}
	values["claude_code_oauth_token"] = token
	return nil
}

func mintClaudeCodeOAuthToken(opts CreateOptions) (string, error) {
	stdout := opts.Stdout
	if stdout == nil {
		stdout = os.Stdout
	}
	if opts.NoInput {
		fmt.Fprintln(stdout, "claude-bin: --no-input set; skipping `claude setup-token`. Set --set claude_code_oauth_token=... or edit .env later.")
		return "", nil
	}
	if !isInteractiveReader(opts.Stdin) {
		fmt.Fprintln(stdout, "claude-bin: non-interactive stdin; skipping `claude setup-token`. Edit .env or pass --set claude_code_oauth_token=...")
		return "", nil
	}
	if _, err := exec.LookPath("claude"); err != nil {
		fmt.Fprintln(stdout, "claude-bin: `claude` CLI not found on host; install Claude Code (https://claude.ai/code) to mint a token, or paste one below.")
		return promptClaudeOAuthTokenPaste(opts)
	}
	if !confirmRunClaudeSetupToken(opts) {
		return promptClaudeOAuthTokenPaste(opts)
	}
	fmt.Fprintln(stdout, "claude-bin: running `claude setup-token` (a browser may open for authentication)...")
	out, err := claudeSetupTokenRunner(opts)
	if err != nil {
		fmt.Fprintf(stdout, "claude-bin: `claude setup-token` failed: %v\n", err)
		return promptClaudeOAuthTokenPaste(opts)
	}
	token := extractClaudeOAuthToken(out)
	if token == "" {
		fmt.Fprintln(stdout, "claude-bin: could not extract token from `claude setup-token` output; paste it below.")
		return promptClaudeOAuthTokenPaste(opts)
	}
	return token, nil
}

func defaultClaudeSetupTokenRunner(opts CreateOptions) (string, error) {
	stdout := opts.Stdout
	if stdout == nil {
		stdout = os.Stdout
	}
	stdin := opts.Stdin
	if stdin == nil {
		stdin = os.Stdin
	}
	if out, ok, err := runClaudeSetupTokenInPTY(stdin, stdout); ok {
		return out, err
	}
	return runClaudeSetupTokenBuffered(stdin, stdout)
}

func runClaudeSetupTokenBuffered(stdin io.Reader, stdout io.Writer) (string, error) {
	cmd := exec.Command("claude", "setup-token")
	cmd.Stdin = stdin
	cmd.Stderr = os.Stderr
	var buf bytes.Buffer
	cmd.Stdout = io.MultiWriter(stdout, &buf)
	if err := cmd.Run(); err != nil {
		return buf.String(), err
	}
	return buf.String(), nil
}

func confirmRunClaudeSetupToken(opts CreateOptions) bool {
	stdin := opts.Stdin
	if stdin == nil {
		stdin = os.Stdin
	}
	stdout := opts.Stdout
	if stdout == nil {
		stdout = os.Stdout
	}
	if !isInteractiveReader(stdin) {
		return false
	}
	fmt.Fprint(stdout, "Run `claude setup-token` now to mint a long-lived OAuth token? [Y/n]: ")
	line, err := bufio.NewReader(stdin).ReadString('\n')
	if err != nil && strings.TrimSpace(line) == "" {
		return false
	}
	answer := strings.ToLower(strings.TrimSpace(line))
	return answer == "" || answer == "y" || answer == "yes"
}

func extractClaudeOAuthToken(out string) string {
	if m := claudeOAuthTokenPattern.FindString(out); m != "" {
		return m
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}
		if !strings.ContainsAny(line, " \t") && len(line) >= 32 {
			return line
		}
		break
	}
	return ""
}

func promptClaudeOAuthTokenPaste(opts CreateOptions) (string, error) {
	stdin := opts.Stdin
	if stdin == nil {
		stdin = os.Stdin
	}
	stdout := opts.Stdout
	if stdout == nil {
		stdout = os.Stdout
	}
	if !isInteractiveReader(stdin) {
		return "", nil
	}
	fmt.Fprint(stdout, "Paste your Claude Code OAuth token (leave empty to skip): ")
	if f, ok := stdin.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		raw, err := term.ReadPassword(int(f.Fd()))
		fmt.Fprintln(stdout)
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(string(raw)), nil
	}
	line, err := bufio.NewReader(stdin).ReadString('\n')
	if err != nil && strings.TrimSpace(line) == "" {
		return "", nil
	}
	return strings.TrimSpace(line), nil
}

func writeNewFile(path string, data []byte, mode os.FileMode) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	_, writeErr := bytes.NewReader(data).WriteTo(file)
	closeErr := file.Close()
	if writeErr != nil {
		return writeErr
	}
	return closeErr
}

func safeTemplateTargetPath(rel string) (string, error) {
	if rel == "" {
		return "", fmt.Errorf("template file path must not be empty")
	}
	fromSlash := filepath.FromSlash(rel)
	if filepath.IsAbs(fromSlash) {
		return "", fmt.Errorf("template file path %q must be relative", rel)
	}
	clean := filepath.Clean(fromSlash)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("template file path %q escapes target directory", rel)
	}
	return clean, nil
}
