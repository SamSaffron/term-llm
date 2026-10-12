package awsauth

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const metadataCredentials = `{"AccessKeyId":"metadata-id","SecretAccessKey":"metadata-secret","Token":"metadata-token","Expiration":"2030-01-02T03:04:05Z"}`
const imdsCredentials = `{"Code":"Success","AccessKeyId":"metadata-id","SecretAccessKey":"metadata-secret","Token":"metadata-token","Expiration":"2030-01-02T03:04:05Z"}`

func TestIMDSv2(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path == "/latest/api/token" {
			if r.Method != "PUT" || r.Header.Get("X-aws-ec2-metadata-token-ttl-seconds") != "21600" {
				t.Errorf("token request %s %v", r.Method, r.Header)
			}
			io.WriteString(w, "imds-token")
			return
		}
		if r.Method != "GET" || r.Header.Get("X-aws-ec2-metadata-token") != "imds-token" {
			t.Errorf("metadata request %s %v", r.Method, r.Header)
		}
		switch r.URL.Path {
		case "/latest/meta-data/iam/security-credentials/":
			io.WriteString(w, "test-role\nignored-role\n")
		case "/latest/meta-data/iam/security-credentials/test-role":
			io.WriteString(w, imdsCredentials)
		default:
			t.Error("unexpected path", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	o := testOptions(t, map[string]string{"AWS_EC2_METADATA_SERVICE_ENDPOINT": server.URL}, "", "")
	o.HTTPClient = server.Client()
	r, err := Resolve(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 0 {
		t.Fatal("IMDS not lazy")
	}
	for i := 0; i < 2; i++ {
		c, err := r.Credentials.Retrieve(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if c.AccessKeyID != "metadata-id" || c.SessionToken != "metadata-token" || c.Source != "ec2 imds" || !c.CanExpire {
			t.Error("wrong metadata credentials")
		}
	}
	if calls.Load() != 3 {
		t.Error("credentials not cached", calls.Load())
	}
}

func TestIMDSFailureAndNoV1Fallback(t *testing.T) {
	cases := []struct {
		name, token, roles, creds string
		tokenStatus               int
		want                      string
		calls                     int32
	}{
		{"token-rejected", "", "", "", 403, "IMDSv1 fallback is disabled", 1},
		{"empty-token", "", "", "", 200, "empty IMDSv2 token", 1},
		{"empty-role", "token", "\n", "", 200, "no IAM role", 2},
		{"code-failure", "token", "role", `{"Code":"SECRET-FAILURE"}`, 200, "not Success", 3},
		{"invalid-json", "token", "role", `SECRET-STDOUT`, 200, "invalid credential JSON", 3},
		{"invalid-expiration", "token", "role", `{"Code":"Success","AccessKeyId":"id","SecretAccessKey":"secret","Expiration":"SECRET-FAILURE"}`, 200, "invalid credential JSON", 3},
		{"missing-secret", "token", "role", `{"Code":"Success","AccessKeyId":"SECRET-ID","Expiration":"2030-01-01T00:00:00Z"}`, 200, "missing AccessKeyId", 3},
		{"missing-expiration", "token", "role", `{"Code":"Success","AccessKeyId":"SECRET-ID","SecretAccessKey":"SECRET-KEY"}`, 200, "missing Expiration", 3},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				switch r.URL.Path {
				case "/latest/api/token":
					if r.Method != "PUT" {
						t.Error("not PUT")
					}
					w.WriteHeader(c.tokenStatus)
					io.WriteString(w, c.token)
				case "/latest/meta-data/iam/security-credentials/":
					if r.Header.Get("X-aws-ec2-metadata-token") == "" {
						t.Error("v1 attempted")
					}
					io.WriteString(w, c.roles)
				default:
					io.WriteString(w, c.creds)
				}
			}))
			defer server.Close()
			o := testOptions(t, nil, "", "")
			o.IMDSEndpoint = server.URL
			o.HTTPClient = server.Client()
			r, err := Resolve(context.Background(), o)
			if err != nil {
				t.Fatal(err)
			}
			_, err = r.Credentials.Retrieve(context.Background())
			if err == nil || !strings.Contains(err.Error(), c.want) || !strings.Contains(err.Error(), "no AWS credentials found") {
				t.Errorf("error = %v", err)
			}
			if calls.Load() != c.calls {
				t.Errorf("calls=%d want %d", calls.Load(), c.calls)
			}
			assertNoMetadataSecrets(t, err)
		})
	}
}

func TestIMDSDisabled(t *testing.T) {
	o := testOptions(t, map[string]string{"AWS_EC2_METADATA_DISABLED": "TRUE"}, "", "")
	o.HTTPClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("network attempted when disabled")
		return nil, nil
	})}
	r, err := Resolve(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	_, err = r.Credentials.Retrieve(context.Background())
	if err == nil || !strings.Contains(err.Error(), "AWS_EC2_METADATA_DISABLED") {
		t.Error(err)
	}
}

func TestContainerCredentials(t *testing.T) {
	for _, relative := range []bool{false, true} {
		for _, tokenFile := range []bool{false, true} {
			t.Run(strings.Join([]string{boolName(relative), boolName(tokenFile)}, "-"), func(t *testing.T) {
				var calls atomic.Int32
				wantToken := "env-token"
				if tokenFile {
					wantToken = "file-token"
				}
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					if r.Method != "GET" || r.URL.Path != "/credentials" || r.Header.Get("Authorization") != wantToken {
						t.Errorf("request = %s %s %v", r.Method, r.URL.Path, r.Header)
					}
					io.WriteString(w, metadataCredentials)
				}))
				defer server.Close()
				env := map[string]string{"AWS_CONTAINER_AUTHORIZATION_TOKEN": "env-token"}
				if relative {
					env["AWS_CONTAINER_CREDENTIALS_RELATIVE_URI"] = "/credentials"
					env["AWS_CONTAINER_CREDENTIALS_FULL_URI"] = "http://8.8.8.8/ignored"
				} else {
					env["AWS_CONTAINER_CREDENTIALS_FULL_URI"] = server.URL + "/credentials"
				}
				if tokenFile {
					file := filepath.Join(t.TempDir(), "token")
					if err := os.WriteFile(file, []byte("file-token"), 0600); err != nil {
						t.Fatal(err)
					}
					env["AWS_CONTAINER_AUTHORIZATION_TOKEN_FILE"] = file
				}
				o := testOptions(t, env, "", "")
				o.ContainerHost = server.URL
				o.HTTPClient = server.Client()
				r, err := Resolve(context.Background(), o)
				if err != nil {
					t.Fatal(err)
				}
				if calls.Load() != 0 {
					t.Fatal("container not lazy")
				}
				for i := 0; i < 2; i++ {
					c, err := r.Credentials.Retrieve(context.Background())
					if err != nil {
						t.Fatal(err)
					}
					if c.AccessKeyID != "metadata-id" || c.SessionToken != "metadata-token" || c.Source != "ecs container" || !c.CanExpire {
						t.Error("wrong container credentials")
					}
				}
				if calls.Load() != 1 {
					t.Error("container not cached", calls.Load())
				}
			})
		}
	}
}

func boolName(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

func TestContainerURIValidation(t *testing.T) {
	cases := []struct {
		uri             string
		relative, valid bool
	}{
		{"http://127.0.0.1:123/creds", false, true}, {"http://[::1]/creds", false, true},
		{"http://169.254.170.2/creds", false, true}, {"http://169.254.170.23/creds", false, true},
		{"http://[fd00:ec2::23]/creds", false, true}, {"https://example.com/creds", false, true},
		{"http://8.8.8.8/creds", false, false}, {"file:///creds", false, false}, {"ftp://127.0.0.1/creds", false, false},
		{"https://user:SECRET-KEY@example.com/creds", false, false}, {"http://127.0.0.1/creds#fragment", false, false},
		{"/creds?foo=bar", true, true}, {"//8.8.8.8/creds", true, false}, {"https://8.8.8.8/creds", true, false}, {"creds", true, false}, {"/%broken", true, false},
	}
	for _, c := range cases {
		env := map[string]string{}
		if c.relative {
			env["AWS_CONTAINER_CREDENTIALS_RELATIVE_URI"] = c.uri
		} else {
			env["AWS_CONTAINER_CREDENTIALS_FULL_URI"] = c.uri
		}
		o := defaultOptions(testOptions(t, env, "", ""))
		_, err := containerEndpoint(context.Background(), o)
		if (err == nil) != c.valid {
			t.Errorf("uri=%q error=%v", c.uri, err)
		}
		assertNoMetadataSecrets(t, err)
	}
}

func TestContainerFailureNeverLeaks(t *testing.T) {
	for _, response := range []string{`SECRET-STDOUT`, `{"AccessKeyId":"SECRET-ID","SecretAccessKey":"SECRET-KEY"}`, `{"AccessKeyId":"SECRET-ID","Expiration":"2030-01-01T00:00:00Z"}`} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, response) }))
		o := testOptions(t, map[string]string{"AWS_CONTAINER_CREDENTIALS_FULL_URI": server.URL}, "", "")
		o.HTTPClient = server.Client()
		r, err := Resolve(context.Background(), o)
		if err != nil {
			t.Fatal(err)
		}
		_, err = r.Credentials.Retrieve(context.Background())
		if err == nil {
			t.Error("expected error")
		}
		assertNoMetadataSecrets(t, err)
		server.Close()
	}
}

func assertNoMetadataSecrets(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		return
	}
	for _, s := range []string{"SECRET-STDOUT", "SECRET-FAILURE", "SECRET-ID", "SECRET-KEY"} {
		if strings.Contains(err.Error(), s) {
			t.Errorf("error leaked %q", s)
		}
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestMetadataRequestTimeout(t *testing.T) {
	o := defaultOptions(testOptions(t, nil, "", ""))
	o.HTTPClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		deadline, ok := r.Context().Deadline()
		if !ok || time.Until(deadline) > time.Second {
			t.Error("IMDS timeout missing or too long")
		}
		<-r.Context().Done()
		return nil, r.Context().Err()
	})}
	_, err := imdsProvider(o).Retrieve(context.Background())
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("timeout = %v", err)
	}
}

func TestMetadataRedirectNotFollowed(t *testing.T) {
	var calls atomic.Int32
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); io.WriteString(w, metadataCredentials) }))
	defer destination.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, destination.URL, 302) }))
	defer source.Close()
	o := testOptions(t, map[string]string{"AWS_CONTAINER_CREDENTIALS_FULL_URI": source.URL, "AWS_CONTAINER_AUTHORIZATION_TOKEN": "SECRET-KEY"}, "", "")
	o.HTTPClient = source.Client()
	r, err := Resolve(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	_, err = r.Credentials.Retrieve(context.Background())
	if err == nil || calls.Load() != 0 {
		t.Error("redirect followed", err, calls.Load())
	}
	assertNoMetadataSecrets(t, err)
}

func TestPinnedContainerClientRejectsDisallowedPeer(t *testing.T) {
	client := pinnedContainerClient(&http.Client{})
	resp, err := client.Get("http://192.0.2.1:9/creds")
	if err == nil {
		resp.Body.Close()
		t.Fatal("expected connection to a non-allowed address to be refused")
	}
	if !strings.Contains(err.Error(), "disallowed address") {
		t.Fatalf("error = %v, want disallowed address", err)
	}

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("ok")) }))
	defer ts.Close()
	resp, err = client.Get(ts.URL)
	if err != nil {
		t.Fatalf("loopback request: %v", err)
	}
	resp.Body.Close()
}

func TestIMDSDefaultEndpointMode(t *testing.T) {
	for mode, want := range map[string]string{"": "http://169.254.169.254", "IPv4": "http://169.254.169.254", "IPv6": "http://[fd00:ec2::254]", "ipv6": "http://[fd00:ec2::254]"} {
		got, err := imdsDefaultEndpoint(mode)
		if err != nil || got != want {
			t.Errorf("imdsDefaultEndpoint(%q) = %q, %v; want %q", mode, got, err, want)
		}
	}
	if _, err := imdsDefaultEndpoint("ipv5"); err == nil {
		t.Error("invalid endpoint mode accepted")
	}
	// An explicit endpoint still wins over the mode.
	var hit atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hit.Store(true); w.WriteHeader(500) }))
	defer server.Close()
	o := testOptions(t, map[string]string{"AWS_EC2_METADATA_SERVICE_ENDPOINT": server.URL, "AWS_EC2_METADATA_SERVICE_ENDPOINT_MODE": "ipv5-typo"}, "", "")
	o.HTTPClient = server.Client()
	_, _ = retrieveIMDS(context.Background(), defaultOptions(o))
	if !hit.Load() {
		t.Error("explicit endpoint blocked by an invalid endpoint mode")
	}
}
