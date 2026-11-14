package update

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func setReleaseBaseURL(t *testing.T, url string) {
	t.Helper()
	old := releaseBaseURL
	releaseBaseURL = url
	t.Cleanup(func() { releaseBaseURL = old })
}

func TestFetchLatestRelease(t *testing.T) {
	t.Run("parses tag from 302 redirect", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/term-llm/term-llm/releases/latest" {
				t.Errorf("release request path = %q, want organization repository", r.URL.Path)
			}
			w.Header().Set("Location", "http://"+r.Host+"/term-llm/term-llm/releases/tag/v1.5.0")
			w.WriteHeader(http.StatusFound)
		}))
		defer srv.Close()
		setReleaseBaseURL(t, srv.URL)

		info, err := FetchLatestRelease(context.Background())
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if info.TagName != "v1.5.0" {
			t.Fatalf("got TagName=%q, want %q", info.TagName, "v1.5.0")
		}
	})

	t.Run("parses tag from 301 redirect", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Location", "http://"+r.Host+"/term-llm/term-llm/releases/tag/v2.0.1")
			w.WriteHeader(http.StatusMovedPermanently)
		}))
		defer srv.Close()
		setReleaseBaseURL(t, srv.URL)

		info, err := FetchLatestRelease(context.Background())
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if info.TagName != "v2.0.1" {
			t.Fatalf("got TagName=%q, want %q", info.TagName, "v2.0.1")
		}
	})

	t.Run("parses tag from relative Location", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Location", "/term-llm/term-llm/releases/tag/v3.0.0")
			w.WriteHeader(http.StatusFound)
		}))
		defer srv.Close()
		setReleaseBaseURL(t, srv.URL)

		info, err := FetchLatestRelease(context.Background())
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if info.TagName != "v3.0.0" {
			t.Fatalf("got TagName=%q, want %q", info.TagName, "v3.0.0")
		}
	})

	t.Run("error on non-redirect status", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNotFound)
		}))
		defer srv.Close()
		setReleaseBaseURL(t, srv.URL)

		_, err := FetchLatestRelease(context.Background())
		if err == nil {
			t.Fatal("expected error for non-redirect status")
		}
	})

	t.Run("error on missing Location header", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusFound)
		}))
		defer srv.Close()
		setReleaseBaseURL(t, srv.URL)

		_, err := FetchLatestRelease(context.Background())
		if err == nil {
			t.Fatal("expected error for missing Location header")
		}
	})

	t.Run("error on unexpected redirect path", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Location", "http://"+r.Host+"/login")
			w.WriteHeader(http.StatusFound)
		}))
		defer srv.Close()
		setReleaseBaseURL(t, srv.URL)

		_, err := FetchLatestRelease(context.Background())
		if err == nil {
			t.Fatal("expected error for unexpected redirect path")
		}
	})
}

func TestNormalizeVersion(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{name: "trim v prefix", input: "v1.2.3", want: "1.2.3"},
		{name: "strip suffix", input: "1.2.3-beta1", want: "1.2.3"},
		{name: "whitespace", input: "  v2.0  ", want: "2.0"},
		{name: "non-numeric", input: "dev", want: ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := NormalizeVersion(tc.input); got != tc.want {
				t.Fatalf("NormalizeVersion(%q)=%q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

func TestCompareVersionStrings(t *testing.T) {
	tests := []struct {
		name     string
		a        string
		b        string
		wantCmp  int
		wantOkay bool
	}{
		{name: "equal different lengths", a: "1.2", b: "1.2.0", wantCmp: 0, wantOkay: true},
		{name: "less than", a: "1.2.3", b: "1.10.0", wantCmp: -1, wantOkay: true},
		{name: "greater than", a: "2.0", b: "1.9.9", wantCmp: 1, wantOkay: true},
		{name: "invalid", a: "1.a", b: "1.2.3", wantCmp: 0, wantOkay: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cmp, ok := CompareVersionStrings(tc.a, tc.b)
			if ok != tc.wantOkay {
				t.Fatalf("CompareVersionStrings(%q,%q) ok=%v, want %v", tc.a, tc.b, ok, tc.wantOkay)
			}
			if !ok {
				return
			}
			if cmp != tc.wantCmp {
				t.Fatalf("CompareVersionStrings(%q,%q)=%d, want %d", tc.a, tc.b, cmp, tc.wantCmp)
			}
		})
	}
}
