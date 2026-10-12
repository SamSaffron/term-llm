package awsauth

import (
	"context"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// Credentials holds AWS signing credentials. Never include it in logs or errors.
type Credentials struct {
	AccessKeyID, SecretAccessKey, SessionToken string
	CanExpire                                  bool
	Expires                                    time.Time
	Source                                     string
}

type CredentialsProvider interface {
	Retrieve(ctx context.Context) (Credentials, error)
}

// Options configures lazy credential resolution. Optional functions are test
// seams; nil functions use the corresponding operating-system implementation.
type Options struct {
	Region, Profile                            string
	AccessKeyID, SecretAccessKey, SessionToken string
	HTTPClient                                 *http.Client
	Getenv                                     func(string) string
	HomeDir                                    func() (string, error)
	RunCommand                                 func(ctx context.Context, name string, args []string, env []string) ([]byte, error)
	LookPath                                   func(string) (string, error)
	Now                                        func() time.Time
	IMDSEndpoint, ContainerHost                string
}

type Resolved struct {
	Region       string
	RegionSource string
	Credentials  CredentialsProvider
}

// Resolve reads shared configuration and chooses a credential provider. It does
// not execute processes, perform DNS lookups, or fetch credentials from a network.
// Only an explicitly selected missing profile or invalid shared file is an error.
func Resolve(ctx context.Context, opts Options) (*Resolved, error) {
	opts = defaultOptions(opts)
	profile := firstNonempty(opts.Profile, opts.Getenv("AWS_PROFILE"), opts.Getenv("AWS_DEFAULT_PROFILE"), "default")
	shared, err := loadSharedProfile(opts, profile)
	if err != nil {
		return nil, err
	}
	region, source := resolveRegion(opts, shared.configRegion)
	provider := chooseCredentials(opts, profile, shared)
	return &Resolved{Region: region, RegionSource: source, Credentials: &credentialsCache{provider: provider, now: opts.Now}}, nil
}

func defaultOptions(o Options) Options {
	if o.Getenv == nil {
		o.Getenv = os.Getenv
	}
	if o.HomeDir == nil {
		o.HomeDir = os.UserHomeDir
	}
	if o.RunCommand == nil {
		o.RunCommand = runCredentialCommand
	}
	if o.LookPath == nil {
		o.LookPath = exec.LookPath
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.HTTPClient == nil {
		o.HTTPClient = newMetadataHTTPClient()
	}
	return o
}

func firstNonempty(values ...string) string {
	for _, s := range values {
		if s != "" {
			return s
		}
	}
	return ""
}

func resolveRegion(o Options, configRegion string) (string, string) {
	candidates := []struct{ value, source string }{
		{o.Region, "static config"}, {o.Getenv("AWS_REGION"), "AWS_REGION"},
		{o.Getenv("AWS_DEFAULT_REGION"), "AWS_DEFAULT_REGION"}, {configRegion, "shared config file"},
	}
	for _, c := range candidates {
		if c.value != "" {
			return c.value, c.source
		}
	}
	return "", ""
}

type retrieveFunc func(context.Context) (Credentials, error)

func (f retrieveFunc) Retrieve(ctx context.Context) (Credentials, error) { return f(ctx) }

func staticProvider(creds Credentials) CredentialsProvider {
	return retrieveFunc(func(context.Context) (Credentials, error) { return creds, nil })
}

func chooseCredentials(o Options, profile string, shared sharedProfile) CredentialsProvider {
	if o.AccessKeyID != "" && o.SecretAccessKey != "" {
		return staticProvider(Credentials{AccessKeyID: o.AccessKeyID, SecretAccessKey: o.SecretAccessKey, SessionToken: o.SessionToken, Source: "static config"})
	}
	if o.Profile == "" {
		env := Credentials{AccessKeyID: firstNonempty(o.Getenv("AWS_ACCESS_KEY_ID"), o.Getenv("AWS_ACCESS_KEY")), SecretAccessKey: firstNonempty(o.Getenv("AWS_SECRET_ACCESS_KEY"), o.Getenv("AWS_SECRET_KEY")), SessionToken: o.Getenv("AWS_SESSION_TOKEN"), Source: "env"}
		if env.AccessKeyID != "" && env.SecretAccessKey != "" {
			return staticProvider(env)
		}
		if o.Getenv("AWS_WEB_IDENTITY_TOKEN_FILE") != "" {
			// Environment web identity (for example EKS IRSA) must not pin a
			// profile, or the CLI ignores the AWS_ROLE_ARN/token environment.
			return cliProvider(o, "", "web identity")
		}
	}
	v := shared.values
	if feature := profileDelegationFeature(v); feature != "" {
		return cliProvider(o, profile, feature)
	}
	if v["aws_access_key_id"] != "" && v["aws_secret_access_key"] != "" {
		return staticProvider(Credentials{AccessKeyID: v["aws_access_key_id"], SecretAccessKey: v["aws_secret_access_key"], SessionToken: v["aws_session_token"], Source: shared.credentialsSource + " (profile " + profile + ")"})
	}
	if v["credential_process"] != "" {
		return processProvider(o, v["credential_process"])
	}
	if o.Getenv("AWS_CONTAINER_CREDENTIALS_RELATIVE_URI") != "" || o.Getenv("AWS_CONTAINER_CREDENTIALS_FULL_URI") != "" {
		return containerProvider(o)
	}
	return imdsProvider(o)
}

func profileDelegationFeature(values map[string]string) string {
	features := []struct {
		keys        []string
		description string
	}{
		{[]string{"role_arn", "source_profile", "credential_source"}, "assume-role"},
		{[]string{"sso_session", "sso_start_url", "sso_account_id"}, "SSO"},
		{[]string{"web_identity_token_file"}, "web identity"},
		{[]string{"login_session"}, "login session"},
	}
	var found []string
	for _, f := range features {
		for _, key := range f.keys {
			if values[key] != "" {
				found = append(found, f.description)
				break
			}
		}
	}
	return strings.Join(found, "/")
}

// newMetadataHTTPClient returns the client used for link-local metadata
// endpoints. It never uses an HTTP proxy: metadata must be fetched directly.
func newMetadataHTTPClient() *http.Client {
	if t, ok := http.DefaultTransport.(*http.Transport); ok {
		t = t.Clone()
		t.Proxy = nil
		return &http.Client{Transport: t}
	}
	return &http.Client{}
}

// Serialize refreshes; successful values are reusable until five minutes before
// expiry. In-flight results (including errors) are shared with concurrent callers,
// but failed retrievals are never cached for subsequent calls.
type credentialsCache struct {
	mu       sync.Mutex
	provider CredentialsProvider
	now      func() time.Time
	value    Credentials
	valid    bool
	flight   *credentialRefresh
}

type credentialRefresh struct {
	done  chan struct{}
	value Credentials
	err   error
}

func (c *credentialsCache) Retrieve(ctx context.Context) (Credentials, error) {
	if err := ctx.Err(); err != nil {
		return Credentials{}, err
	}
	c.mu.Lock()
	if c.valid && (!c.value.CanExpire || c.now().Before(c.value.Expires.Add(-5*time.Minute))) {
		value := c.value
		c.mu.Unlock()
		return value, nil
	}
	flight := c.flight
	if flight == nil {
		flight = &credentialRefresh{done: make(chan struct{})}
		c.flight = flight
		// The refresh is detached from the caller that started it so one
		// caller's cancellation cannot fail unrelated concurrent requests.
		go c.refresh(context.WithoutCancel(ctx), flight)
	}
	c.mu.Unlock()
	select {
	case <-ctx.Done():
		return Credentials{}, ctx.Err()
	case <-flight.done:
		return flight.value, flight.err
	}
}

// credentialRefreshTimeout bounds a detached refresh; providers apply their
// own tighter limits (60s for commands, seconds for metadata endpoints).
const credentialRefreshTimeout = 2 * time.Minute

func (c *credentialsCache) refresh(ctx context.Context, flight *credentialRefresh) {
	ctx, cancel := context.WithTimeout(ctx, credentialRefreshTimeout)
	defer cancel()
	value, err := c.provider.Retrieve(ctx)
	c.mu.Lock()
	defer c.mu.Unlock()
	if err == nil {
		c.value, c.valid = value, true
		flight.value = value
	}
	flight.err = err
	c.flight = nil
	close(flight.done)
}
