package llm

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"testing"
	"time"
)

// ProxyFromEnvironment caches its environment process-wide. A fresh test process
// exercises the default client's real environment proxy without depending on
// which other tests have already initialized that cache.
func TestReadURLToolRedirectCheckPreservesEnvironmentProxy(t *testing.T) {
	const scenarioEnv = "TERM_LLM_READ_URL_PROXY_TEST_SCENARIO"
	if scenario := os.Getenv(scenarioEnv); scenario != "" {
		testReadURLToolEnvironmentProxy(t, scenario)
		return
	}

	for _, scenario := range []string{"direct_http", "http_proxy", "https_proxy", "no_proxy"} {
		t.Run(scenario, func(t *testing.T) {
			for _, key := range []string{
				"HTTP_PROXY", "http_proxy", "HTTPS_PROXY", "https_proxy",
				"ALL_PROXY", "all_proxy", "NO_PROXY", "no_proxy", "REQUEST_METHOD",
			} {
				t.Setenv(key, "")
			}
			if scenario == "http_proxy" || scenario == "no_proxy" {
				t.Setenv("HTTP_PROXY", "http://user:pass@192.0.2.10:3128")
			}
			if scenario == "no_proxy" {
				t.Setenv("NO_PROXY", "attacker.example")
			}
			if scenario == "https_proxy" {
				t.Setenv("HTTPS_PROXY", "http://user:pass@192.0.2.10:3128")
			}
			t.Setenv(scenarioEnv, scenario)

			cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestReadURLToolRedirectCheckPreservesEnvironmentProxy$")
			if output, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("environment proxy regression: %v\n%s", err, output)
			}
		})
	}
}

type readURLProxyPacket struct {
	address   string
	method    string
	host      string
	proxyAuth string
	err       error
}

func testReadURLToolEnvironmentProxy(t *testing.T, scenario string) {
	t.Helper()
	const targetIP = "203.0.113.20"
	const proxyAddress = "192.0.2.10:3128"
	const targetHost = "attacker.example"
	const proxyAuth = "user:pass"

	scheme := "http"
	wantAddress := targetIP + ":80"
	wantMethod := http.MethodGet
	wantHost := targetHost
	wantAuth := ""
	if scenario == "http_proxy" || scenario == "https_proxy" {
		wantAddress = proxyAddress
		wantAuth = "Basic " + base64.StdEncoding.EncodeToString([]byte(proxyAuth))
	}
	if scenario == "https_proxy" {
		scheme = "https"
		wantMethod = http.MethodConnect
		wantHost += ":443"
	}

	originalLookup := readURLLookupIP
	readURLLookupIP = func(ctx context.Context, host string) ([]net.IP, error) {
		if host != targetHost {
			return nil, fmt.Errorf("unexpected DNS lookup for %q", host)
		}
		return []net.IP{net.ParseIP(targetIP)}, nil
	}
	t.Cleanup(func() { readURLLookupIP = originalLookup })

	packets := make(chan readURLProxyPacket, 1)
	originalTransport := http.DefaultTransport
	transport := originalTransport.(*http.Transport).Clone()
	// Keep ProxyFromEnvironment intact and replace only the final dial. The
	// in-memory peer records exactly what would go to the selected endpoint;
	// neither the proxy nor the target is contacted over the network.
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		client, peer := net.Pipe()
		_ = client.SetDeadline(time.Now().Add(5 * time.Second))
		_ = peer.SetDeadline(time.Now().Add(5 * time.Second))
		go func() {
			defer peer.Close()
			req, err := http.ReadRequest(bufio.NewReader(peer))
			if err != nil {
				packets <- readURLProxyPacket{address: address, err: err}
				return
			}
			defer req.Body.Close()
			packets <- readURLProxyPacket{
				address:   address,
				method:    req.Method,
				host:      req.Host,
				proxyAuth: req.Header.Get("Proxy-Authorization"),
			}
			status := "200 OK"
			if req.Method == http.MethodConnect {
				// Credentials are transmitted before any target TLS handshake.
				// Reject CONNECT so this test needs no TLS server or certificates.
				status = "502 Bad Gateway"
			}
			_, _ = fmt.Fprintf(peer, "HTTP/1.1 %s\r\nContent-Length: 0\r\nConnection: close\r\n\r\n", status)
		}()
		return client, nil
	}
	http.DefaultTransport = transport
	t.Cleanup(func() {
		transport.CloseIdleConnections()
		http.DefaultTransport = originalTransport
	})

	fetcher := &stubURLFetcher{body: "fetched through configured provider"}
	tool := NewReadURLToolWithFetcher(fetcher)
	tool.client.Timeout = 5 * time.Second
	targetURL := scheme + "://" + targetHost + "/article"
	args, err := json.Marshal(map[string]string{"url": targetURL})
	if err != nil {
		t.Fatal(err)
	}
	out, executeErr := tool.Execute(t.Context(), args)

	select {
	case packet := <-packets:
		if packet.err != nil {
			t.Fatalf("read outbound request to %s: %v", packet.address, packet.err)
		}
		if packet.method != wantMethod || packet.host != wantHost {
			t.Errorf("outbound request = %s %s, want %s %s", packet.method, packet.host, wantMethod, wantHost)
		}
		if packet.proxyAuth != wantAuth {
			t.Errorf("Proxy-Authorization = %q, want %q", packet.proxyAuth, wantAuth)
		}
		if packet.address != wantAddress {
			t.Errorf("redirect check dialed %s, want %s; environment proxy routing was bypassed", packet.address, wantAddress)
			if packet.proxyAuth != "" {
				t.Errorf("proxy credentials were sent to non-proxy endpoint %s in %s %s", packet.address, packet.method, packet.host)
			}
		}
	default:
		// Failing closed before transmitting anything is safe, but a request
		// that succeeds must exercise the configured route.
		if executeErr == nil {
			t.Fatal("redirect check succeeded without an outbound request")
		}
	}

	if scenario == "https_proxy" {
		if executeErr == nil {
			t.Fatal("expected the test proxy's rejected CONNECT to fail the redirect check")
		}
		if fetcher.gotURL != "" {
			t.Errorf("fetcher called after rejected CONNECT with %q", fetcher.gotURL)
		}
	} else {
		if executeErr != nil {
			t.Fatalf("Execute: %v", executeErr)
		}
		if fetcher.gotURL != targetURL || out.Content != fetcher.body {
			t.Fatalf("fetcher got URL %q and output %q, want %q and %q", fetcher.gotURL, out.Content, targetURL, fetcher.body)
		}
	}
}
