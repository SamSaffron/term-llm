package contain

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/samsaffron/term-llm/internal/clipboard"
)

type Runner interface {
	Run(ctx context.Context, name string, args []string, opts RunOptions) error
	Output(ctx context.Context, name string, args []string, opts RunOptions) ([]byte, error)
}

type RunOptions struct {
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
	Dir    string
}

var consoleEnvNames = []string{
	"TERM",
	"COLORTERM",
	"CLICOLOR",
	"CLICOLOR_FORCE",
	"FORCE_COLOR",
	"NO_COLOR",
}

const (
	containPrimarySelectionProxyEnableEnv = "TERM_LLM_ENABLE_PRIMARY_SELECTION_PROXY"
	containPrimarySelectionURLEnv         = "TERM_LLM_PRIMARY_SELECTION_URL"
)

type OSRunner struct{}

func (OSRunner) Run(ctx context.Context, name string, args []string, opts RunOptions) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdin = opts.Stdin
	cmd.Stdout = opts.Stdout
	cmd.Stderr = opts.Stderr
	cmd.Dir = opts.Dir
	return cmd.Run()
}

func (OSRunner) Output(ctx context.Context, name string, args []string, opts RunOptions) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdin = opts.Stdin
	cmd.Stderr = opts.Stderr
	cmd.Dir = opts.Dir
	return cmd.Output()
}

func ComposeBaseArgs(name string) ([]string, string, error) {
	if err := ValidateName(name); err != nil {
		return nil, "", err
	}
	dir, err := ContainerDir(name)
	if err != nil {
		return nil, "", err
	}
	compose, err := ComposePath(name)
	if err != nil {
		return nil, "", err
	}
	return []string{"compose", "-f", compose, "--project-directory", dir, "-p", ProjectName(name)}, dir, nil
}

func Start(ctx context.Context, runner Runner, name string, stdout, stderr io.Writer) error {
	args, dir, err := ComposeBaseArgs(name)
	if err != nil {
		return err
	}
	if err := ensureComposeDefinition(name); err != nil {
		return err
	}
	if err := syncManagedImagesForWorkspace(name); err != nil {
		return err
	}
	args = append(args, "up", "-d", "--build")
	return runner.Run(ctx, "docker", args, RunOptions{Stdout: stdout, Stderr: stderr, Dir: dir})
}

func Stop(ctx context.Context, runner Runner, name string, stdout, stderr io.Writer) error {
	args, dir, err := ComposeBaseArgs(name)
	if err != nil {
		return err
	}
	if err := ensureComposeDefinition(name); err != nil {
		return err
	}
	args = append(args, "stop")
	return runner.Run(ctx, "docker", args, RunOptions{Stdout: stdout, Stderr: stderr, Dir: dir})
}

// Remove permanently deletes a contain workspace: Compose resources are brought
// down with volumes removed, then the workspace definition directory is deleted.
func Remove(ctx context.Context, runner Runner, name string, stdout, stderr io.Writer) error {
	args, dir, err := ComposeBaseArgs(name)
	if err != nil {
		return err
	}
	if err := ensureComposeDefinitionExists(name); err != nil {
		return err
	}
	args = append(args, "down", "--volumes", "--remove-orphans")
	if err := runner.Run(ctx, "docker", args, RunOptions{Stdout: stdout, Stderr: stderr, Dir: dir}); err != nil {
		return err
	}
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("remove contain workspace directory %s: %w", dir, err)
	}
	return nil
}

// Rebuild rebuilds workspace images without cache, pulls newer base images where
// Compose can, and recreates containers while preserving volumes/networks.
func Rebuild(ctx context.Context, runner Runner, name string, stdout, stderr io.Writer) error {
	args, dir, err := ComposeBaseArgs(name)
	if err != nil {
		return err
	}
	if err := ensureComposeDefinition(name); err != nil {
		return err
	}
	if err := syncManagedImagesForWorkspace(name); err != nil {
		return err
	}
	buildArgs := append(append([]string{}, args...), "build", "--pull", "--no-cache")
	if err := runner.Run(ctx, "docker", buildArgs, RunOptions{Stdout: stdout, Stderr: stderr, Dir: dir}); err != nil {
		return err
	}
	upArgs := append(append([]string{}, args...), "up", "-d", "--force-recreate")
	return runner.Run(ctx, "docker", upArgs, RunOptions{Stdout: stdout, Stderr: stderr, Dir: dir})
}

type ShellOptions struct {
	User string
}

func Exec(ctx context.Context, runner Runner, name string, cmdArgs []string, stdin io.Reader, stdout, stderr io.Writer) error {
	if len(cmdArgs) == 0 {
		return Shell(ctx, runner, name, stdin, stdout, stderr)
	}
	forceRaw := false
	if cmdArgs[0] == "--" {
		forceRaw = true
		cmdArgs = cmdArgs[1:]
		if len(cmdArgs) == 0 {
			return Shell(ctx, runner, name, stdin, stdout, stderr)
		}
	}
	info, dir, err := composeInfoForCommand(name)
	if err != nil {
		return err
	}
	if !forceRaw {
		if recipe, ok := info.Hints.ExecRecipes[cmdArgs[0]]; ok {
			cmdArgs = append(append([]string{}, recipe.Command...), cmdArgs[1:]...)
		}
	}
	args, _, err := ComposeBaseArgs(name)
	if err != nil {
		return err
	}
	service := info.DefaultService()
	proxyEnv, cleanup := startPrimarySelectionProxy(ctx, runner, args, service, dir)
	defer cleanup()
	args = append(args, "exec")
	user := info.DefaultUser(service)
	if user != "" {
		args = append(args, "--user", user)
		args = appendContainUserEnvironment(args, user, info.Hints.Workspace)
	}
	args = appendConsoleEnvExecArgs(args)
	args = append(args, proxyEnv...)
	args = append(args, service)
	args = append(args, cmdArgs...)
	return runner.Run(ctx, "docker", args, RunOptions{Stdin: stdin, Stdout: stdout, Stderr: stderr, Dir: dir})
}

func Shell(ctx context.Context, runner Runner, name string, stdin io.Reader, stdout, stderr io.Writer) error {
	return ShellWithOptions(ctx, runner, name, ShellOptions{}, stdin, stdout, stderr)
}

func ShellWithOptions(ctx context.Context, runner Runner, name string, opts ShellOptions, stdin io.Reader, stdout, stderr io.Writer) error {
	info, dir, err := composeInfoForCommand(name)
	if err != nil {
		return err
	}
	args, _, err := ComposeBaseArgs(name)
	if err != nil {
		return err
	}
	service := info.DefaultService()
	proxyEnv, cleanup := startPrimarySelectionProxy(ctx, runner, args, service, dir)
	defer cleanup()
	args = append(args, "exec")
	user := strings.TrimSpace(opts.User)
	if user == "" {
		user = info.DefaultUser(service)
	}
	if user != "" {
		args = append(args, "--user", user)
		args = appendContainUserEnvironment(args, user, info.Hints.Workspace)
	}
	args = appendConsoleEnvExecArgs(args)
	args = append(args, proxyEnv...)
	args = append(args, service, info.Shell())
	return runner.Run(ctx, "docker", args, RunOptions{Stdin: stdin, Stdout: stdout, Stderr: stderr, Dir: dir})
}

func appendContainUserEnvironment(args []string, user, workspace string) []string {
	workspace = strings.TrimSpace(workspace)
	if user == "" || user == "root" || workspace == "" || !strings.HasPrefix(workspace, "/") {
		return args
	}
	args = append(args, "--workdir", workspace)
	args = append(args, "-e", "HOME="+workspace)
	args = append(args, "-e", "USER="+user)
	args = append(args, "-e", "LOGNAME="+user)
	return args
}

func appendConsoleEnvExecArgs(args []string) []string {
	for _, name := range consoleEnvNames {
		value := os.Getenv(name)
		if value == "" {
			continue
		}
		args = append(args, "-e", name+"="+value)
	}
	return args
}

func startPrimarySelectionProxy(ctx context.Context, runner Runner, composeArgs []string, service, dir string) ([]string, func()) {
	if !primarySelectionProxyEnabled() {
		return nil, func() {}
	}
	gateway, ok := containerGateway(ctx, runner, composeArgs, service, dir)
	if !ok {
		return nil, func() {}
	}
	listener, err := net.Listen("tcp", net.JoinHostPort(gateway, "0"))
	if err != nil {
		return nil, func() {}
	}
	token, err := randomHexToken(16)
	if err != nil {
		_ = listener.Close()
		return nil, func() {}
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/primary", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("token") != token && r.Header.Get("X-Term-LLM-Token") != token {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		text, err := clipboard.ReadPrimarySelection()
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, text)
	})
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 2 * time.Second}
	go func() { _ = server.Serve(listener) }()

	addr := listener.Addr().(*net.TCPAddr)
	url := fmt.Sprintf("http://%s/primary?token=%s", net.JoinHostPort(addr.IP.String(), fmt.Sprint(addr.Port)), token)
	cleanup := func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}
	return []string{
		"-e", containPrimarySelectionURLEnv + "=" + url,
	}, cleanup
}

func primarySelectionProxyEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(containPrimarySelectionProxyEnableEnv))) {
	case "1", "true", "yes", "on", "y":
		return true
	default:
		return false
	}
}

func containerGateway(ctx context.Context, runner Runner, composeArgs []string, service, dir string) (string, bool) {
	psArgs := append(append([]string{}, composeArgs...), "ps", "-q", service)
	containerIDBytes, err := runner.Output(ctx, "docker", psArgs, RunOptions{Dir: dir})
	if err != nil {
		return "", false
	}
	containerID := strings.Fields(string(containerIDBytes))
	if len(containerID) == 0 {
		return "", false
	}
	inspectArgs := []string{"inspect", "-f", "{{range .NetworkSettings.Networks}}{{println .Gateway}}{{end}}", containerID[0]}
	gatewayBytes, err := runner.Output(ctx, "docker", inspectArgs, RunOptions{Dir: dir})
	if err != nil {
		return "", false
	}
	for _, field := range strings.Fields(string(gatewayBytes)) {
		if ip := net.ParseIP(field); ip != nil {
			return ip.String(), true
		}
	}
	return "", false
}

func randomHexToken(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

func DockerPS(ctx context.Context, runner Runner, stderr io.Writer) ([]byte, error) {
	args := []string{"ps", "-a", "--filter", "label=org.term-llm.contain=true", "--format", "{{json .}}"}
	return runner.Output(ctx, "docker", args, RunOptions{Stderr: stderr})
}

func syncManagedImagesForWorkspace(name string) error {
	dir, err := ContainerDir(name)
	if err != nil {
		return err
	}
	compose, err := ComposePath(name)
	if err != nil {
		return err
	}
	info, err := ReadComposeInfo(compose)
	if err != nil || info.Invalid {
		return nil
	}
	agentDir, err := ImageDir("agent")
	if err != nil {
		return err
	}
	for _, service := range info.Services {
		if !sameBuildContext(service.BuildContext, agentDir, dir) {
			continue
		}
		if _, err := SyncImage("agent", true); err != nil {
			return fmt.Errorf("sync managed agent image: %w", err)
		}
		return nil
	}
	return nil
}

func sameBuildContext(contextPath, targetPath, composeDir string) bool {
	contextPath = strings.TrimSpace(contextPath)
	if contextPath == "" {
		return false
	}
	if !filepath.IsAbs(contextPath) {
		contextPath = filepath.Join(composeDir, contextPath)
	}
	contextAbs, err := filepath.Abs(contextPath)
	if err == nil {
		contextPath = contextAbs
	}
	targetAbs, err := filepath.Abs(targetPath)
	if err == nil {
		targetPath = targetAbs
	}
	return filepath.Clean(contextPath) == filepath.Clean(targetPath)
}

func ensureComposeDefinition(name string) error {
	path, err := ComposePath(name)
	if err != nil {
		return err
	}
	if _, err := ReadComposeInfo(path); err != nil {
		return fmt.Errorf("contain workspace %q has no compose.yaml at %s: %w", name, path, err)
	}
	return nil
}

func ensureComposeDefinitionExists(name string) error {
	path, err := ComposePath(name)
	if err != nil {
		return err
	}
	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("contain workspace %q has no compose.yaml at %s: %w", name, path, err)
	}
	return nil
}

func composeInfoForCommand(name string) (ComposeInfo, string, error) {
	if err := ValidateName(name); err != nil {
		return ComposeInfo{}, "", err
	}
	dir, err := ContainerDir(name)
	if err != nil {
		return ComposeInfo{}, "", err
	}
	path, err := ComposePath(name)
	if err != nil {
		return ComposeInfo{}, "", err
	}
	info, err := ReadComposeInfo(path)
	if err != nil {
		return ComposeInfo{}, "", fmt.Errorf("contain workspace %q has no compose.yaml at %s: %w", name, path, err)
	}
	if info.Invalid {
		return ComposeInfo{}, "", fmt.Errorf("contain workspace %q has invalid compose.yaml: %s", name, info.InvalidReason)
	}
	return info, dir, nil
}
