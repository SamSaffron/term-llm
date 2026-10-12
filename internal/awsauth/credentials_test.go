package awsauth

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testOptions(t *testing.T, env map[string]string, config, credentials string) Options {
	t.Helper()
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".aws"), 0700); err != nil {
		t.Fatal(err)
	}
	if config != "" {
		if err := os.WriteFile(filepath.Join(home, ".aws", "config"), []byte(config), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if credentials != "" {
		if err := os.WriteFile(filepath.Join(home, ".aws", "credentials"), []byte(credentials), 0600); err != nil {
			t.Fatal(err)
		}
	}
	return Options{Getenv: func(k string) string { return env[k] }, HomeDir: func() (string, error) { return home, nil }}
}

func TestResolveCredentialPrecedence(t *testing.T) {
	cases := []struct {
		name                                                               string
		env                                                                map[string]string
		config, credentials, profile, staticKey, staticSecret, staticToken string
		wantKey, wantToken, wantSource                                     string
	}{
		{name: "explicit-static", env: map[string]string{"AWS_ACCESS_KEY_ID": "env-id", "AWS_SECRET_ACCESS_KEY": "env-secret"}, staticKey: "explicit-id", staticSecret: "explicit-secret", staticToken: "explicit-token", credentials: "[default]\naws_access_key_id = profile-id\naws_secret_access_key = profile-secret\n", wantKey: "explicit-id", wantToken: "explicit-token", wantSource: "static config"},
		{name: "env-beats-default", env: map[string]string{"AWS_ACCESS_KEY_ID": "env-id", "AWS_SECRET_ACCESS_KEY": "env-secret", "AWS_SESSION_TOKEN": "env-token"}, credentials: "[default]\naws_access_key_id = profile-id\naws_secret_access_key = profile-secret\n", wantKey: "env-id", wantToken: "env-token", wantSource: "env"},
		{name: "legacy-env", env: map[string]string{"AWS_ACCESS_KEY": "legacy-id", "AWS_SECRET_KEY": "legacy-secret"}, wantKey: "legacy-id", wantSource: "env"},
		{name: "modern-env-beats-legacy", env: map[string]string{"AWS_ACCESS_KEY_ID": "env-id", "AWS_SECRET_ACCESS_KEY": "env-secret", "AWS_ACCESS_KEY": "legacy-id", "AWS_SECRET_KEY": "legacy-secret"}, wantKey: "env-id", wantSource: "env"},
		{name: "explicit-profile-beats-env", profile: "work", env: map[string]string{"AWS_ACCESS_KEY_ID": "env-id", "AWS_SECRET_ACCESS_KEY": "env-secret"}, credentials: "[work]\naws_access_key_id = work-id\naws_secret_access_key = work-secret\naws_session_token = work-token\n", wantKey: "work-id", wantToken: "work-token", wantSource: "shared credentials file (profile work)"},
		{name: "env-profile-does-not-beat-env-keys", env: map[string]string{"AWS_PROFILE": "work", "AWS_ACCESS_KEY_ID": "env-id", "AWS_SECRET_ACCESS_KEY": "env-secret"}, credentials: "[work]\naws_access_key_id = work-id\naws_secret_access_key = work-secret\n", wantKey: "env-id", wantSource: "env"},
		{name: "env-profile", env: map[string]string{"AWS_PROFILE": "work"}, config: "[profile work]\naws_access_key_id = config-id\naws_secret_access_key = config-secret\n", wantKey: "config-id", wantSource: "shared config file (profile work)"},
		{name: "credentials-beat-config", config: "[default]\naws_access_key_id = config-id\naws_secret_access_key = config-secret\naws_session_token = config-token\n", credentials: "[default]\naws_access_key_id = file-id\naws_secret_access_key = file-secret\naws_session_token = file-token\n", wantKey: "file-id", wantToken: "file-token", wantSource: "shared credentials file (profile default)"},
		{name: "incomplete-env-falls-through", env: map[string]string{"AWS_ACCESS_KEY_ID": "incomplete-id"}, credentials: "[default]\naws_access_key_id = file-id\naws_secret_access_key = file-secret\n", wantKey: "file-id", wantSource: "shared credentials file (profile default)"},
		{name: "incomplete-explicit-falls-through", staticKey: "incomplete-id", env: map[string]string{"AWS_ACCESS_KEY_ID": "env-id", "AWS_SECRET_ACCESS_KEY": "env-secret"}, wantKey: "env-id", wantSource: "env"},
		{name: "static-file-beats-process", credentials: "[default]\naws_access_key_id = file-id\naws_secret_access_key = file-secret\ncredential_process = not-executed\n", wantKey: "file-id", wantSource: "shared credentials file (profile default)"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			o := testOptions(t, c.env, c.config, c.credentials)
			o.Profile = c.profile
			o.AccessKeyID, o.SecretAccessKey, o.SessionToken = c.staticKey, c.staticSecret, c.staticToken
			o.RunCommand = func(context.Context, string, []string, []string) ([]byte, error) {
				t.Fatal("unexpected command")
				return nil, nil
			}
			o.LookPath = func(string) (string, error) { t.Fatal("unexpected lookpath"); return "", nil }
			r, err := Resolve(context.Background(), o)
			if err != nil {
				t.Fatal(err)
			}
			got, err := r.Credentials.Retrieve(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if got.AccessKeyID != c.wantKey || got.SessionToken != c.wantToken || got.Source != c.wantSource || got.CanExpire {
				t.Errorf("credentials key=%q token=%q source=%q expire=%v", got.AccessKeyID, got.SessionToken, got.Source, got.CanExpire)
			}
		})
	}
}

func TestResolveRegionPrecedence(t *testing.T) {
	cases := []struct {
		name, region         string
		env                  map[string]string
		config, want, source string
	}{
		{"explicit", "ap-southeast-2", map[string]string{"AWS_REGION": "env-region", "AWS_DEFAULT_REGION": "default-region"}, "[default]\nregion = file-region\n", "ap-southeast-2", "static config"},
		{"aws-region", "", map[string]string{"AWS_REGION": "env-region", "AWS_DEFAULT_REGION": "default-region"}, "[default]\nregion = file-region\n", "env-region", "AWS_REGION"},
		{"default-region", "", map[string]string{"AWS_DEFAULT_REGION": "default-region"}, "[default]\nregion = file-region\n", "default-region", "AWS_DEFAULT_REGION"},
		{"profile", "", map[string]string{"AWS_PROFILE": "work"}, "[default]\nregion = wrong-region\n[profile work]\nregion = file-region\n", "file-region", "shared config file"},
		{"empty", "", nil, "", "", ""},
		{"ignore-credentials-region", "", nil, "[default]\nregion = file-region\n", "file-region", "shared config file"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			o := testOptions(t, c.env, c.config, "[default]\nregion = credentials-region\n")
			o.Region = c.region
			r, err := Resolve(context.Background(), o)
			if err != nil {
				t.Fatal(err)
			}
			if r.Region != c.want || r.RegionSource != c.source {
				t.Errorf("region=%q source=%q", r.Region, r.RegionSource)
			}
		})
	}
}

func TestResolveMissingProfile(t *testing.T) {
	for _, fromEnv := range []bool{false, true} {
		env := map[string]string{}
		o := testOptions(t, env, "", "")
		if fromEnv {
			env["AWS_PROFILE"] = "missing"
		} else {
			o.Profile = "missing"
		}
		_, err := Resolve(context.Background(), o)
		var missing *SharedConfigProfileNotExistError
		if !errors.As(err, &missing) || missing.Profile != "missing" {
			t.Errorf("missing profile error = %v", err)
		}
	}
	o := testOptions(t, map[string]string{"AWS_EC2_METADATA_DISABLED": "true"}, "", "")
	r, err := Resolve(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = r.Credentials.Retrieve(context.Background()); err == nil || !strings.Contains(err.Error(), "no AWS credentials found") {
		t.Errorf("retrieve missing default = %v", err)
	}
}

func TestResolveLazyDelegation(t *testing.T) {
	cases := []struct {
		name, setting string
		env           map[string]string
	}{
		{"role", "role_arn = role", nil}, {"source-profile", "source_profile = source", nil},
		{"credential-source", "credential_source = Environment", nil}, {"sso-session", "sso_session = session", nil},
		{"sso-start", "sso_start_url = https://sso", nil}, {"sso-account", "sso_account_id = account", nil},
		{"webidentity-profile", "web_identity_token_file = token-path", nil}, {"login", "login_session = session", nil},
		{"webidentity-env", "", map[string]string{"AWS_WEB_IDENTITY_TOKEN_FILE": "token-path"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			o := testOptions(t, c.env, "[default]\n"+c.setting+"\naws_access_key_id = must-not-use\naws_secret_access_key = must-not-use\n", "")
			lookCalls, runCalls := 0, 0
			o.LookPath = func(name string) (string, error) {
				lookCalls++
				if name != "aws" {
					t.Error(name)
				}
				return "fake-aws", nil
			}
			o.RunCommand = func(ctx context.Context, name string, args, env []string) ([]byte, error) {
				runCalls++
				want := "configure export-credentials --profile default --format process"
				if c.name == "webidentity-env" {
					// Environment web identity must not pin a profile.
					want = "configure export-credentials --format process"
				}
				if name != "fake-aws" || strings.Join(args, " ") != want {
					t.Errorf("command %q %v", name, args)
				}
				if _, ok := ctx.Deadline(); !ok {
					t.Error("no timeout")
				}
				if len(env) == 0 {
					t.Error("environment not inherited")
				}
				return []byte(`{"Version":1,"AccessKeyId":"cli-id","SecretAccessKey":"cli-secret"}`), nil
			}
			r, err := Resolve(context.Background(), o)
			if err != nil {
				t.Fatal(err)
			}
			if lookCalls != 0 || runCalls != 0 {
				t.Fatal("Resolve executed a command")
			}
			creds, err := r.Credentials.Retrieve(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if lookCalls != 1 || runCalls != 1 || creds.AccessKeyID != "cli-id" || creds.Source != "aws cli export-credentials" {
				t.Error("wrong CLI retrieval")
			}
		})
	}
}

func TestResolveCLIMissing(t *testing.T) {
	o := testOptions(t, nil, "[profile work]\nsso_session = session\n", "")
	o.Profile = "work"
	o.LookPath = func(string) (string, error) { return "", errors.New("not found") }
	r, err := Resolve(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	_, err = r.Credentials.Retrieve(context.Background())
	for _, s := range []string{`profile "work"`, "SSO", "AWS CLI v2", "aws configure export-credentials", "access_key_id/secret_access_key"} {
		if err == nil || !strings.Contains(err.Error(), s) {
			t.Errorf("error = %v missing %q", err, s)
		}
	}
}

func TestAWSDefaultProfileSelection(t *testing.T) {
	creds := "[default]\naws_access_key_id = default-id\naws_secret_access_key = s\n[prod]\naws_access_key_id = prod-id\naws_secret_access_key = s\n[work]\naws_access_key_id = work-id\naws_secret_access_key = s\n"
	for _, tc := range []struct {
		env  map[string]string
		want string
	}{
		{map[string]string{"AWS_DEFAULT_PROFILE": "prod"}, "prod-id"},
		{map[string]string{"AWS_DEFAULT_PROFILE": "prod", "AWS_PROFILE": "work"}, "work-id"},
		{nil, "default-id"},
	} {
		r, err := Resolve(context.Background(), testOptions(t, tc.env, "", creds))
		if err != nil {
			t.Fatal(err)
		}
		c, err := r.Credentials.Retrieve(context.Background())
		if err != nil || c.AccessKeyID != tc.want {
			t.Errorf("env %v: access key = %q (%v), want %q", tc.env, c.AccessKeyID, err, tc.want)
		}
	}
	// A missing profile named through AWS_DEFAULT_PROFILE is an explicit selection.
	_, err := Resolve(context.Background(), testOptions(t, map[string]string{"AWS_DEFAULT_PROFILE": "missing"}, "", creds))
	var missing *SharedConfigProfileNotExistError
	if !errors.As(err, &missing) {
		t.Fatalf("error = %v, want SharedConfigProfileNotExistError", err)
	}
}
