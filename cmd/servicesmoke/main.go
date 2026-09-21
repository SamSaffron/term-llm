// Command servicesmoke exercises installed service specifications and real
// runners without touching a supervisor.
//
// Usage: go run ./cmd/servicesmoke ./term-llm
//
// Build the binary under test first (make build). All account state and native
// service files stay under ./tmp.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/samsaffron/term-llm/internal/userservice"
)

const configYAML = "default_provider: debug\nproviders:\n  debug:\n    model: fast\n"

// registrationSecret is a fixture credential; it must never reach a log.
const registrationSecret = "service-smoke-registration-secret"

// smoke owns the isolated environment and the single runner under test.
type smoke struct {
	binary  string
	root    string
	env     []string
	running *runner
	log     string
}

// runner tracks one started process; exactly one goroutine waits on it.
type runner struct {
	cmd  *exec.Cmd
	done chan struct{}
	err  error
}

func main() {
	if runtime.GOOS != "linux" {
		fail("this isolated file-backed smoke is Linux-only; it must not create real Keychain items")
	}
	binary := "./term-llm"
	if len(os.Args) > 1 {
		binary = os.Args[1]
	}
	s, err := newSmoke(binary)
	if err != nil {
		fail("%v", err)
	}
	if err := s.run(); err != nil {
		s.terminate()
		fmt.Fprintf(os.Stderr, "Isolated failure artifacts: %s\n", s.root)
		fail("%v", err)
	}
	if err := os.RemoveAll(s.root); err != nil {
		fail("%v", err)
	}
}

func newSmoke(binary string) (*smoke, error) {
	absolute, err := filepath.Abs(binary)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll("tmp", 0o755); err != nil {
		return nil, err
	}
	root, err := os.MkdirTemp("tmp", "service-smoke.")
	if err != nil {
		return nil, err
	}
	if root, err = filepath.Abs(root); err != nil {
		return nil, err
	}
	env := []string{
		"HOME=" + root,
		"XDG_CONFIG_HOME=" + filepath.Join(root, "config"),
		"XDG_DATA_HOME=" + filepath.Join(root, "data"),
		"XDG_CACHE_HOME=" + filepath.Join(root, "cache"),
	}
	for _, key := range []string{"PATH", "LANG", "USER", "SHELL"} {
		if value, ok := os.LookupEnv(key); ok {
			env = append(env, key+"="+value)
		}
	}
	config := filepath.Join(root, "config/term-llm/config.yaml")
	if err := os.MkdirAll(filepath.Dir(config), 0o755); err != nil {
		return nil, err
	}
	if err := os.WriteFile(config, []byte(configYAML), 0o600); err != nil {
		return nil, err
	}
	return &smoke{binary: absolute, root: root, env: env}, nil
}

// run performs every phase, stopping at the first failed expectation.
func (s *smoke) run() error {
	if err := s.passkeyPhase(); err != nil {
		return err
	}
	if err := s.bearerPhase(); err != nil {
		return err
	}
	return s.hubPhase()
}

// passkeyPhase proves the managed Web runner consumes the private enrollment
// capability without disclosing it in the log.
func (s *smoke) passkeyPhase() error {
	port, err := freePort()
	if err != nil {
		return err
	}
	public := fmt.Sprintf("http://localhost:%d/ui/", port)
	if _, err := s.cli("install", "web", "--no-start", "--yes", "--", "--port", strconv.Itoa(port),
		"--public-url", public, "--disable-widgets", "--disable-extensions", "--no-projects"); err != nil {
		return err
	}
	var enrollment struct {
		Secret string `json:"secret"`
	}
	if err := readJSON(filepath.Join(s.specDir("web"), "enrollment.json"), &enrollment); err != nil {
		return err
	}
	local, err := s.start("web")
	if err != nil {
		return err
	}
	status, _, err := s.request(public+"api/auth/bootstrap/verify", "", map[string]string{"code": enrollment.Secret})
	if err != nil {
		return err
	}
	if err := expect(status == 200, "bootstrap verify status = %d, want 200", status); err != nil {
		return err
	}
	status, _, err = s.request(local+"v1/models", "", nil)
	if err != nil {
		return err
	}
	if err := expect(status == 401, "unauthenticated models status = %d, want 401", status); err != nil {
		return err
	}
	logged, err := os.ReadFile(s.log)
	if err != nil {
		return err
	}
	if err := expect(!bytes.Contains(logged, []byte(enrollment.Secret)), "enrollment secret disclosed in the runner log"); err != nil {
		return err
	}
	if err := s.stop(); err != nil {
		return err
	}
	fmt.Println("PASS: managed Web passkey runner consumes private enrollment capability; no log disclosure")
	return nil
}

// bearerPhase proves a custom bearer Web service keeps stable credentials across
// reinstall, authenticates the API, and keeps the token out of log and spec.
func (s *smoke) bearerPhase() error {
	port, err := freePort()
	if err != nil {
		return err
	}
	if _, err := s.cli("install", "web", "--no-start", "--yes", "--", "--auth", "bearer", "--port", strconv.Itoa(port),
		"--disable-widgets", "--disable-extensions", "--no-projects"); err != nil {
		return err
	}
	token, err := s.token("web")
	if err != nil {
		return err
	}
	if _, err := s.cli("install", "web", "--no-start", "--yes"); err != nil {
		return err
	}
	reinstalled, err := s.token("web")
	if err != nil {
		return err
	}
	if err := expect(reinstalled == token, "reinstall rotated the bearer credential"); err != nil {
		return err
	}
	local, err := s.start("web")
	if err != nil {
		return err
	}
	status, _, err := s.request(local+"v1/models", token, nil)
	if err != nil {
		return err
	}
	if err := expect(status == 200, "authenticated models status = %d, want 200", status); err != nil {
		return err
	}
	if err := s.absent("web", token, "bearer token"); err != nil {
		return err
	}
	if err := s.stop(); err != nil {
		return err
	}
	fmt.Println("PASS: custom bearer Web service, stable credentials on reinstall, authenticated API")
	return nil
}

// hubPhase proves an independent managed Hub runner keeps its registration
// credential private and shuts down cleanly on SIGTERM.
func (s *smoke) hubPhase() error {
	port, err := freePort()
	if err != nil {
		return err
	}
	secretFile := filepath.Join(s.root, "import.env")
	if err := os.WriteFile(secretFile, []byte("TERM_LLM_HUB_REGISTRATION_TOKEN="+registrationSecret+"\n"), 0o600); err != nil {
		return err
	}
	if _, err := s.cli("install", "hub", "--no-start", "--yes", "--secrets-file", secretFile, "--",
		"--auth", "bearer", "--port", strconv.Itoa(port), "--contain=false"); err != nil {
		return err
	}
	token, err := s.token("hub")
	if err != nil {
		return err
	}
	local, err := s.start("hub")
	if err != nil {
		return err
	}
	status, _, err := s.request(local+"api/nodes", token, nil)
	if err != nil {
		return err
	}
	if err := expect(status == 200, "authenticated nodes status = %d, want 200", status); err != nil {
		return err
	}
	status, _, err = s.request(local+"api/nodes", "", nil)
	if err != nil {
		return err
	}
	if err := expect(status == 401, "unauthenticated nodes status = %d, want 401", status); err != nil {
		return err
	}
	status, body, err := s.request(local+"api/registration-info", token, nil)
	if err != nil {
		return err
	}
	var info struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.Unmarshal(body, &info); err != nil {
		return err
	}
	if err := expect(status == 200 && info.Enabled, "registration info = %d %+v", status, info); err != nil {
		return err
	}
	if err := s.absent("hub", token, "bearer token"); err != nil {
		return err
	}
	if err := s.absent("hub", registrationSecret, "registration credential"); err != nil {
		return err
	}
	if err := s.stop(); err != nil {
		return err
	}
	fmt.Println("PASS: independent managed Hub runner, private registration credential, clean SIGTERM")
	return nil
}

func (s *smoke) specDir(kind string) string {
	return filepath.Join(s.root, "config/term-llm/services", kind)
}

func (s *smoke) specPath(kind string) string {
	return filepath.Join(s.specDir(kind), "service.json")
}

// cli runs one `term-llm service` subcommand in the isolated environment.
func (s *smoke) cli(args ...string) (string, error) {
	command := exec.Command(s.binary, append([]string{"service"}, args...)...)
	command.Env = s.env
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		return "", fmt.Errorf("service %s failed: %v: %s", args[0], err, stderr.String())
	}
	return stdout.String(), nil
}

func (s *smoke) token(kind string) (string, error) {
	out, err := s.cli("token", kind)
	return strings.TrimSpace(out), err
}

// absent fails when a credential appears in the runner log or the installed spec.
func (s *smoke) absent(kind, secret, what string) error {
	for _, path := range []string{s.log, s.specPath(kind)} {
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if err := expect(!strings.Contains(string(data), secret), "%s disclosed in %s", what, path); err != nil {
			return err
		}
	}
	return nil
}

// start launches the runner for kind and waits for its health endpoint.
func (s *smoke) start(kind string) (string, error) {
	var spec userservice.Spec
	if err := readJSON(s.specPath(kind), &spec); err != nil {
		return "", err
	}
	s.log = filepath.Join(s.root, kind+".log")
	output, err := os.Create(s.log)
	if err != nil {
		return "", err
	}
	defer output.Close()
	command := exec.Command(s.binary, "service", "run", kind, "--spec", s.specPath(kind))
	command.Env = s.env
	command.Stdout = output
	command.Stderr = output
	if err := command.Start(); err != nil {
		return "", err
	}
	active := &runner{cmd: command, done: make(chan struct{})}
	s.running = active
	go func() {
		active.err = command.Wait()
		close(active.done)
	}()

	local := fmt.Sprintf("http://127.0.0.1:%d%s/", spec.Port, spec.BasePath)
	for range 100 {
		select {
		case <-active.done:
			logged, _ := os.ReadFile(s.log)
			s.running = nil
			return "", fmt.Errorf("%s runner exited: %s", kind, logged)
		default:
		}
		if status, _, err := s.request(local+"healthz", "", nil); err == nil && status == 200 {
			return local, nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return "", fmt.Errorf("%s runner failed health check", kind)
}

// stop requires a clean SIGTERM shutdown from the running runner.
func (s *smoke) stop() error {
	active := s.running
	if active == nil {
		return nil
	}
	s.running = nil
	if err := active.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		return err
	}
	select {
	case <-active.done:
		if active.err != nil {
			return fmt.Errorf("runner did not shut down cleanly: %w", active.err)
		}
		return nil
	case <-time.After(20 * time.Second):
		_ = active.cmd.Process.Kill()
		return fmt.Errorf("runner did not exit within 20s of SIGTERM")
	}
}

// terminate is the failure-path cleanup; it makes no claim about exit status.
func (s *smoke) terminate() {
	active := s.running
	if active == nil {
		return
	}
	s.running = nil
	_ = active.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-active.done:
	case <-time.After(20 * time.Second):
		_ = active.cmd.Process.Kill()
	}
}

// client never consults proxy environment variables; every request is loopback.
var client = &http.Client{
	Timeout:   2 * time.Second,
	Transport: &http.Transport{Proxy: nil},
}

// request performs one API call, returning the status even for error responses.
func (s *smoke) request(url, token string, body any) (int, []byte, error) {
	var payload io.Reader
	method := http.MethodGet
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		payload = bytes.NewReader(encoded)
		method = http.MethodPost
	}
	req, err := http.NewRequest(method, url, payload)
	if err != nil {
		return 0, nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Origin", strings.Split(url, "/ui/")[0])
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	return resp.StatusCode, data, err
}

func freePort() (int, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port, nil
}

func readJSON(path string, value any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, value); err != nil {
		return fmt.Errorf("decode %s: %w", path, err)
	}
	return nil
}

func expect(condition bool, format string, args ...any) error {
	if condition {
		return nil
	}
	return fmt.Errorf(format, args...)
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "service smoke: "+format+"\n", args...)
	os.Exit(1)
}
