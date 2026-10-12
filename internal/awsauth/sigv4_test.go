package awsauth

// Fixtures in testdata/ were generated with aws-sdk-go-v2 as an oracle (core
// v1.47.1, config v1.33.6, eventstream v1.7.20) before the dependency was removed.

import (
	"encoding/json"
	"net/http"
	"os"
	"reflect"
	"testing"
	"time"
)

func TestSignV4Oracle(t *testing.T) {
	var cases []struct {
		Name, Method, URL, RawPath, Opaque, Host string
		ContentLength                            int64
		Headers                                  http.Header
		PayloadHash                              string
		Credentials                              Credentials
		Service, Region                          string
		Time                                     time.Time
		ExpectedHeaders                          http.Header
		ExpectedHost, ExpectedQuery              string
	}
	b, err := os.ReadFile("testdata/sigv4.json")
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(b, &cases); err != nil {
		t.Fatal(err)
	}
	if len(cases) < 15 {
		t.Fatalf("only %d cases", len(cases))
	}
	for _, c := range cases {
		t.Run(c.Name, func(t *testing.T) {
			req, err := http.NewRequest(c.Method, c.URL, nil)
			if err != nil {
				t.Fatal(err)
			}
			req.URL.RawPath, req.URL.Opaque = c.RawPath, c.Opaque
			req.Host, req.ContentLength, req.Header = c.Host, c.ContentLength, c.Headers.Clone()
			if err = SignV4(req, c.PayloadHash, c.Credentials, c.Service, c.Region, c.Time); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(req.Header, c.ExpectedHeaders) {
				t.Errorf("headers\ngot: %#v\nwant: %#v", req.Header, c.ExpectedHeaders)
			}
			if req.Host != c.ExpectedHost {
				t.Errorf("host = %q, want %q", req.Host, c.ExpectedHost)
			}
			if req.URL.RawQuery != c.ExpectedQuery {
				t.Errorf("query = %q, want %q", req.URL.RawQuery, c.ExpectedQuery)
			}
		})
	}
}

func TestSignV4InvalidRequest(t *testing.T) {
	for _, r := range []*http.Request{nil, {}} {
		if SignV4(r, "", Credentials{}, "", "", time.Time{}) == nil {
			t.Error("expected error")
		}
	}
	req, _ := http.NewRequest("GET", "https://example.com", nil)
	req.Header = nil
	if err := SignV4(req, "", Credentials{}, "service", "region", time.Time{}); err != nil {
		t.Fatal(err)
	}
	if req.Header.Get("X-Amz-Content-Sha256") != "" {
		t.Error("signer must not add content sha header")
	}
}
