package llm

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestResolveReadURLTargetPreservesConfiguredProxyEndpoint(t *testing.T) {
	originalLookup := readURLLookupIP
	readURLLookupIP = func(context.Context, string) ([]net.IP, error) {
		return []net.IP{net.ParseIP("203.0.113.20")}, nil
	}
	t.Cleanup(func() { readURLLookupIP = originalLookup })

	for _, scheme := range []string{"http", "https", "socks5", "socks5h"} {
		t.Run(scheme, func(t *testing.T) {
			proxyURL, err := url.Parse(scheme + "://user:pass@192.0.2.10:3128")
			if err != nil {
				t.Fatal(err)
			}
			dials := make(chan string, 1)
			dialErr := errors.New("dial disabled for test")
			client := &http.Client{Transport: &http.Transport{
				Proxy: http.ProxyURL(proxyURL),
				DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
					dials <- address
					return nil, dialErr
				},
			}}
			_, err = resolveReadURLTarget(t.Context(), client, "https://attacker.example/article")
			if !errors.Is(err, dialErr) {
				t.Fatalf("resolve: %v, want test dial error", err)
			}
			select {
			case address := <-dials:
				if address != "192.0.2.10:3128" {
					t.Errorf("proxy route dialed %s, want configured proxy endpoint", address)
				}
			default:
				t.Fatal("proxy route did not dial")
			}
		})
	}
}

func TestResolveReadURLTargetProxySelectionFailureDoesNotDial(t *testing.T) {
	proxyErr := errors.New("proxy selection failed")
	client := &http.Client{Transport: &http.Transport{
		Proxy: func(*http.Request) (*url.URL, error) { return nil, proxyErr },
		DialContext: func(context.Context, string, string) (net.Conn, error) {
			t.Error("proxy selection failure must not fall back to a direct dial")
			return nil, errors.New("unexpected dial")
		},
	}}
	_, err := resolveReadURLTarget(t.Context(), client, "http://203.0.113.20/article")
	if !errors.Is(err, proxyErr) {
		t.Fatalf("resolve: %v, want proxy selection failure", err)
	}
}

func TestResolveReadURLTargetRechecksProxyAndSafetyAtEachHop(t *testing.T) {
	originalLookup := readURLLookupIP
	readURLLookupIP = func(context.Context, string) ([]net.IP, error) {
		return []net.IP{net.ParseIP("203.0.113.20")}, nil
	}
	t.Cleanup(func() { readURLLookupIP = originalLookup })

	for _, blockedRedirect := range []bool{false, true} {
		t.Run(fmt.Sprintf("blocked_redirect=%v", blockedRedirect), func(t *testing.T) {
			proxyURL, err := url.Parse("http://user:pass@192.0.2.10:3128")
			if err != nil {
				t.Fatal(err)
			}
			type observation struct {
				address, host, auth string
				err                 error
			}
			packets := make(chan observation, 2)
			var selectedHosts []string
			client := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{
				Proxy: func(req *http.Request) (*url.URL, error) {
					selectedHosts = append(selectedHosts, req.URL.Host)
					if blockedRedirect || req.URL.Host == "second.example" {
						return proxyURL, nil
					}
					return nil, nil
				},
				DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
					conn, peer := net.Pipe()
					_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
					_ = peer.SetDeadline(time.Now().Add(5 * time.Second))
					go func() {
						defer peer.Close()
						req, err := http.ReadRequest(bufio.NewReader(peer))
						if err != nil {
							packets <- observation{err: err}
							return
						}
						defer req.Body.Close()
						packets <- observation{address: address, host: req.Host, auth: req.Header.Get("Proxy-Authorization")}
						if req.Host == "first.example" {
							location := "http://second.example/final"
							if blockedRedirect {
								location = "http://127.0.0.1/private"
							}
							_, _ = fmt.Fprintf(peer, "HTTP/1.1 302 Found\r\nLocation: %s\r\nContent-Length: 0\r\nConnection: close\r\n\r\n", location)
						} else {
							_, _ = fmt.Fprint(peer, "HTTP/1.1 200 OK\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
						}
					}()
					return conn, nil
				},
			}}
			resolved, err := resolveReadURLTarget(t.Context(), client, "http://first.example/start")
			wantHosts := []string{"first.example", "second.example"}
			wantPackets := []observation{
				{address: "203.0.113.20:80", host: "first.example"},
				{address: "192.0.2.10:3128", host: "second.example", auth: "Basic dXNlcjpwYXNz"},
			}
			if blockedRedirect {
				if err == nil || !strings.Contains(err.Error(), "url host is not allowed") {
					t.Fatalf("resolve private redirect: %v, want blocked-host error", err)
				}
				wantHosts = wantHosts[:1]
				wantPackets = []observation{{address: "192.0.2.10:3128", host: "first.example", auth: "Basic dXNlcjpwYXNz"}}
			} else if err != nil || resolved != "http://second.example/final" {
				t.Fatalf("resolve: url=%q error=%v", resolved, err)
			}
			if !reflect.DeepEqual(selectedHosts, wantHosts) {
				t.Errorf("proxy selection hosts = %v, want %v", selectedHosts, wantHosts)
			}
			var gotPackets []observation
			for len(packets) > 0 {
				gotPackets = append(gotPackets, <-packets)
			}
			if !reflect.DeepEqual(gotPackets, wantPackets) {
				t.Errorf("outbound requests = %+v, want %+v", gotPackets, wantPackets)
			}
		})
	}
}
