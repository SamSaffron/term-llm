package awsauth

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestSharedINI(t *testing.T) {
	config := `; top comment
[default] # section comment
REGION = "us-west-2" ; comment
s3 =
  aws_access_key_id = must-ignore
  region = must-ignore
[profile work]
aws_access_key_id = "config-id"
aws_secret_access_key = 'config-secret'
credential_process = example "arg with spaces"
[sso-session session]
sso_region = ignored
[not-a-profile]
region = ignored
`
	credentials := "[work]\n AWS_ACCESS_KEY_ID = indented-is-subsetting\nAWS_ACCESS_KEY_ID = file-id # comment\nAWS_SECRET_ACCESS_KEY = file-secret\nAWS_SESSION_TOKEN = token#not-comment\n"
	o := testOptions(t, nil, config, credentials)
	p, err := loadSharedProfile(o, "work")
	if err != nil {
		t.Fatal(err)
	}
	for k, want := range map[string]string{"aws_access_key_id": "file-id", "aws_secret_access_key": "file-secret", "aws_session_token": "token#not-comment", "credential_process": `example "arg with spaces"`} {
		if p.values[k] != want {
			t.Errorf("%s = %q, want %q", k, p.values[k], want)
		}
	}
	p, err = loadSharedProfile(o, "default")
	if err != nil {
		t.Fatal(err)
	}
	if p.configRegion != "us-west-2" || p.values["aws_access_key_id"] != "" {
		t.Errorf("default = %#v", p)
	}
}

func TestSharedFileOverrides(t *testing.T) {
	o := testOptions(t, map[string]string{}, "[default]\nregion = wrong\n", "")
	dir := t.TempDir()
	configPath, credsPath := filepath.Join(dir, "config"), filepath.Join(dir, "credentials")
	if err := os.WriteFile(configPath, []byte("[profile work]\nregion = correct\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(credsPath, []byte("[work]\naws_access_key_id = correct-id\naws_secret_access_key = correct-secret\n"), 0600); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"AWS_CONFIG_FILE": configPath, "AWS_SHARED_CREDENTIALS_FILE": credsPath, "AWS_PROFILE": "work"}
	o.Getenv = func(k string) string { return env[k] }
	r, err := Resolve(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	c, err := r.Credentials.Retrieve(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if r.Region != "correct" || c.AccessKeyID != "correct-id" {
		t.Error("overrides not applied")
	}
}

func TestMalformedSharedLinesAreIgnored(t *testing.T) {
	// The AWS SDK ignores lines it cannot tokenize; a stray line must not make
	// an otherwise valid shared file unusable.
	for _, bad := range []string{"[default", "[other] invalid-suffix", "not-valid-SECRET-VALUE"} {
		for _, credentialsFile := range []bool{false, true} {
			body := "[default]\n" + bad + "\naws_access_key_id = good-id\naws_secret_access_key = good-secret\n"
			config, credentials := body, ""
			if credentialsFile {
				config, credentials = "", body
			}
			o := testOptions(t, nil, config, credentials)
			r, err := Resolve(context.Background(), o)
			if err != nil {
				t.Fatalf("bad line %q (credentials file %v): %v", bad, credentialsFile, err)
			}
			c, err := r.Credentials.Retrieve(context.Background())
			if err != nil {
				t.Fatalf("bad line %q (credentials file %v): retrieve: %v", bad, credentialsFile, err)
			}
			if c.AccessKeyID != "good-id" {
				t.Errorf("bad line %q (credentials file %v): access key = %q", bad, credentialsFile, c.AccessKeyID)
			}
		}
	}
}

func TestProfileExistsInEitherFile(t *testing.T) {
	for _, config := range []bool{false, true} {
		c, k := "", "[work]\n"
		if config {
			c, k = "[profile work]\n", ""
		}
		o := testOptions(t, nil, c, k)
		o.Profile = "work"
		if _, err := Resolve(context.Background(), o); err != nil {
			t.Fatal(err)
		}
	}
}

func TestSharedINIIndentedPropertiesAndLegacySeparator(t *testing.T) {
	o := testOptions(t, nil, "[default]\n region: 'us-west-2'\n aws_access_key_id = indented-id\n aws_secret_access_key = indented-secret\ns3 =\n  aws_access_key_id = must-ignore\n  region = must-ignore\n", "")
	r, err := Resolve(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	c, err := r.Credentials.Retrieve(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if r.Region != "us-west-2" || c.AccessKeyID != "indented-id" {
		t.Errorf("indented keys not promoted: region=%q key=%q", r.Region, c.AccessKeyID)
	}
}

func TestSharedFileCannotBeOpenedIsEmpty(t *testing.T) {
	file := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(file, []byte("placeholder"), 0600); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"AWS_CONFIG_FILE": filepath.Join(file, "config"), "AWS_SHARED_CREDENTIALS_FILE": filepath.Join(file, "credentials"), "AWS_ACCESS_KEY_ID": "env-id", "AWS_SECRET_ACCESS_KEY": "env-secret"}
	o := testOptions(t, env, "[default]\nregion = do-not-fall-back\n", "")
	r, err := Resolve(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if r.Region != "" {
		t.Error("fell back to default shared config path")
	}
	c, err := r.Credentials.Retrieve(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if c.AccessKeyID != "env-id" {
		t.Error("env provider not available")
	}
	o.Profile = "work"
	if _, err := Resolve(context.Background(), o); err == nil {
		t.Error("missing explicit profile was not detected")
	}
}
