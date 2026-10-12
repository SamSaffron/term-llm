package awsauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"syscall"
	"time"
)

func containerProvider(o Options) CredentialsProvider {
	return retrieveFunc(func(ctx context.Context) (Credentials, error) {
		endpoint, err := containerEndpoint(ctx, o)
		if err != nil {
			return Credentials{}, fmt.Errorf("ecs container: %w", err)
		}
		token := o.Getenv("AWS_CONTAINER_AUTHORIZATION_TOKEN")
		if file := o.Getenv("AWS_CONTAINER_AUTHORIZATION_TOKEN_FILE"); file != "" {
			data, err := readBoundedFile(file, 64<<10)
			if err != nil {
				return Credentials{}, errors.New("ecs container: cannot read authorization token file")
			}
			token = strings.TrimSpace(string(data))
		}
		headers := make(http.Header)
		if token != "" {
			headers.Set("Authorization", token)
		}
		client := o.HTTPClient
		if strings.HasPrefix(endpoint, "http://") {
			client = pinnedContainerClient(client)
		}
		body, err := metadataRequest(ctx, client, 5*time.Second, "GET", endpoint, headers)
		if err != nil {
			return Credentials{}, fmt.Errorf("ecs container: %w", err)
		}
		var result credentialJSON
		if json.Unmarshal(body, &result) != nil {
			return Credentials{}, errors.New("ecs container: invalid credential JSON")
		}
		return result.credentials("ecs container", true)
	})
}

// readBoundedFile reads at most limit bytes, failing for larger files.
func readBoundedFile(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, errors.New("file too large")
	}
	return data, nil
}

func containerEndpoint(ctx context.Context, o Options) (string, error) {
	relative := o.Getenv("AWS_CONTAINER_CREDENTIALS_RELATIVE_URI")
	if relative != "" {
		base := firstNonempty(o.ContainerHost, "http://169.254.170.2")
		// Never allow the relative URI to replace the trusted host or introduce
		// authority/userinfo. In particular, do not use ResolveReference here.
		uri, err := url.Parse(relative)
		if err != nil || !strings.HasPrefix(relative, "/") || strings.HasPrefix(relative, "//") || uri.IsAbs() || uri.Host != "" || uri.Fragment != "" {
			return "", errors.New("invalid container relative URI")
		}
		return strings.TrimRight(base, "/") + relative, nil
	}
	endpoint := o.Getenv("AWS_CONTAINER_CREDENTIALS_FULL_URI")
	u, err := url.Parse(endpoint)
	if err != nil || u.Hostname() == "" || u.User != nil || u.Fragment != "" {
		return "", errors.New("invalid container credential URI")
	}
	if u.Scheme == "https" {
		return endpoint, nil
	}
	if u.Scheme != "http" {
		return "", errors.New("container credential URI must use https or http on an allowed host")
	}
	if err := allowedContainerHost(ctx, u.Hostname()); err != nil {
		return "", err
	}
	return endpoint, nil
}

func allowedContainerHost(ctx context.Context, host string) error {
	if ip := net.ParseIP(host); ip != nil {
		if allowedContainerIP(ip) {
			return nil
		}
		return errors.New("container credential URI host is not loopback or an ECS/EKS address")
	}
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil || len(ips) == 0 {
		return errors.New("cannot resolve container credential URI host")
	}
	for _, addr := range ips {
		if !allowedContainerIP(addr.IP) {
			return errors.New("container credential URI host is not loopback or an ECS/EKS address")
		}
	}
	return nil
}

// pinnedContainerClient re-checks the peer address at connect time, so a
// hostname that passed allowedContainerHost cannot be rebound to another
// address before the authorization token is sent over plain HTTP. Custom
// round trippers (test seams) are returned unchanged.
func pinnedContainerClient(client *http.Client) *http.Client {
	var base *http.Transport
	switch t := client.Transport.(type) {
	case nil:
		if dt, ok := http.DefaultTransport.(*http.Transport); ok {
			base = dt
		}
	case *http.Transport:
		base = t
	}
	if base == nil {
		return client
	}
	transport := base.Clone()
	transport.Proxy = nil
	dialer := &net.Dialer{Timeout: 5 * time.Second, Control: func(_, address string, _ syscall.RawConn) error {
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			return err
		}
		if ip := net.ParseIP(host); ip == nil || !allowedContainerIP(ip) {
			return errors.New("container credential host resolved to a disallowed address")
		}
		return nil
	}}
	transport.DialContext = dialer.DialContext
	pinned := *client
	pinned.Transport = transport
	return &pinned
}

func allowedContainerIP(ip net.IP) bool {
	return ip.IsLoopback() || ip.Equal(net.ParseIP("169.254.170.2")) || ip.Equal(net.ParseIP("169.254.170.23")) || ip.Equal(net.ParseIP("fd00:ec2::23"))
}

// Never follow metadata redirects (they could exfiltrate authorization headers).
// Bodies and network errors are deliberately not included in errors.
func metadataRequest(ctx context.Context, client *http.Client, timeout time.Duration, method, endpoint string, headers http.Header) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, endpoint, nil)
	if err != nil {
		return nil, errors.New("invalid metadata request")
	}
	req.Header = headers
	safeClient := *client
	safeClient.CheckRedirect = func(*http.Request, []*http.Request) error { return errors.New("metadata redirects are not allowed") }
	resp, err := safeClient.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("metadata request: %w", ctx.Err())
		}
		return nil, errors.New("metadata request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("metadata request returned HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1024*1024+1))
	if err != nil {
		return nil, errors.New("cannot read metadata response")
	}
	if len(body) > 1024*1024 {
		return nil, errors.New("metadata response exceeds 1 MiB")
	}
	return body, nil
}
