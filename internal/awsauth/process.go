package awsauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strings"
	"time"
)

// cliProvider delegates to `aws configure export-credentials`. An empty
// profile lets the CLI use its own environment-driven chain (for example
// AWS_WEB_IDENTITY_TOKEN_FILE + AWS_ROLE_ARN).
func cliProvider(o Options, profile, feature string) CredentialsProvider {
	return retrieveFunc(func(ctx context.Context) (Credentials, error) {
		path, err := o.LookPath("aws")
		if err != nil {
			subject := fmt.Sprintf("profile %q uses %s", profile, feature)
			if profile == "" {
				subject = "AWS_WEB_IDENTITY_TOKEN_FILE " + feature + " credentials need the AWS CLI"
			}
			return Credentials{}, fmt.Errorf("%s; install the AWS CLI v2 so term-llm can resolve it via 'aws configure export-credentials', or configure access_key_id/secret_access_key", subject)
		}
		args := []string{"configure", "export-credentials"}
		if profile != "" {
			args = append(args, "--profile", profile)
		}
		args = append(args, "--format", "process")
		return retrieveCommand(ctx, o, path, args, cliExportSource)
	})
}

const cliExportSource = "aws cli export-credentials"

func processProvider(o Options, command string) CredentialsProvider {
	return retrieveFunc(func(ctx context.Context) (Credentials, error) {
		name, args := "sh", []string{"-c", command}
		if runtime.GOOS == "windows" {
			name, args = "cmd.exe", []string{"/C", command}
		}
		return retrieveCommand(ctx, o, name, args, "credential_process")
	})
}

func retrieveCommand(ctx context.Context, o Options, name string, args []string, source string) (Credentials, error) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	output, err := o.RunCommand(ctx, name, args, os.Environ())
	if err != nil {
		message := source + ": command failed"
		if ctx.Err() != nil {
			return Credentials{}, fmt.Errorf("%s: %w", message, ctx.Err())
		}
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			message += fmt.Sprintf(" (exit status %d)", exit.ExitCode())
			// Only the AWS CLI's diagnostics are surfaced (they explain expired
			// SSO sessions and similar). An arbitrary credential_process may
			// print freshly minted credentials to stderr, so its output is
			// never included.
			if source == cliExportSource {
				if stderr := safeCommandStderr(exit.Stderr, output, o); stderr != "" {
					message += ": " + stderr
				}
			} else {
				message += "; run the command manually to diagnose"
			}
		}
		// Do not expose arbitrary runner errors: they may contain command stdout,
		// arguments or credentials.
		return Credentials{}, errors.New(message)
	}
	var result credentialJSON
	if len(output) > 1024*1024 || json.Unmarshal(output, &result) != nil {
		return Credentials{}, fmt.Errorf("%s: invalid credential JSON", source)
	}
	if result.Version != 1 {
		return Credentials{}, fmt.Errorf("%s: credential JSON Version must be 1", source)
	}
	return result.credentials(source, false)
}

type credentialJSON struct {
	Version         int
	AccessKeyID     string `json:"AccessKeyId"`
	SecretAccessKey string
	SessionToken    string
	Token           string
	Expiration      *time.Time
	Code            string
}

func (j credentialJSON) credentials(source string, requireExpiry bool) (Credentials, error) {
	if j.AccessKeyID == "" || j.SecretAccessKey == "" {
		return Credentials{}, fmt.Errorf("%s: missing AccessKeyId or SecretAccessKey", source)
	}
	if requireExpiry && (j.Expiration == nil || j.Expiration.IsZero()) {
		return Credentials{}, fmt.Errorf("%s: missing Expiration", source)
	}
	c := Credentials{AccessKeyID: j.AccessKeyID, SecretAccessKey: j.SecretAccessKey, SessionToken: firstNonempty(j.SessionToken, j.Token), Source: source}
	if j.Expiration != nil {
		c.CanExpire, c.Expires = true, *j.Expiration
	}
	return c, nil
}

// Capture output without printing stderr or retaining unbounded subprocess data.
func runCredentialCommand(ctx context.Context, name string, args []string, env []string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	// Bound waits on shell descendants that retain inherited output pipes after
	// the command context expires; otherwise exec.Cmd can wait indefinitely.
	cmd.WaitDelay = time.Second
	// No stdin: credential commands run non-interactively. An interactive
	// prompt (MFA, SSO) could never be answered from inside the TUI and would
	// otherwise steal keystrokes until the timeout.
	cmd.Env = env
	stdout, stderr := &boundedCommandBuffer{limit: 1024 * 1024}, &boundedCommandBuffer{limit: 2048}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	err := cmd.Run()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		exit.Stderr = []byte(stderr.text)
	}
	if stdout.overflow && err == nil {
		err = errors.New("credential process output exceeded limit")
	}
	return []byte(stdout.text), err
}

type boundedCommandBuffer struct {
	text     string
	limit    int
	overflow bool
}

func (b *boundedCommandBuffer) Write(p []byte) (int, error) {
	n := len(p)
	remaining := b.limit - len(b.text)
	if len(p) > remaining {
		b.overflow = true
		p = p[:remaining]
	}
	b.text += string(p)
	return n, nil
}

func safeCommandStderr(stderr, stdout []byte, o Options) string {
	text := strings.TrimSpace(string(stderr))
	var output credentialJSON
	_ = json.Unmarshal(stdout, &output)
	secrets := []string{strings.TrimSpace(string(stdout)), o.AccessKeyID, o.SecretAccessKey, o.SessionToken, output.AccessKeyID, output.SecretAccessKey, output.SessionToken, output.Token,
		o.Getenv("AWS_ACCESS_KEY_ID"), o.Getenv("AWS_ACCESS_KEY"), o.Getenv("AWS_SECRET_ACCESS_KEY"), o.Getenv("AWS_SECRET_KEY"), o.Getenv("AWS_SESSION_TOKEN")}
	for _, secret := range secrets {
		if secret != "" {
			text = strings.ReplaceAll(text, secret, "[redacted]")
		}
	}
	text = awsSecretLikeRe.ReplaceAllString(text, "[redacted]")
	if len(text) > 2048 {
		text = text[:2048]
	}
	return text
}

// awsSecretLikeRe matches strings shaped like AWS access key IDs and long
// base64 secrets/session tokens, as a backstop to exact-value redaction.
var awsSecretLikeRe = regexp.MustCompile(`\b(?:AKIA|ASIA|AROA|AIDA)[0-9A-Z]{12,}\b|[A-Za-z0-9+/=]{40,}`)
