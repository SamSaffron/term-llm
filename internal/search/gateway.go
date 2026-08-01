package search

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/gateway/protocol"
)

type GatewayClient struct {
	baseURL string
	token   string
	client  *http.Client
}

func NewGatewayClient(cfg config.GatewayConfig) (*GatewayClient, error) {
	if !cfg.Enabled() {
		return nil, fmt.Errorf("gateway is not configured")
	}
	token, err := cfg.ResolveToken()
	if err != nil {
		return nil, err
	}
	timeout := config.DefaultGatewayResponseTimeout
	if strings.TrimSpace(cfg.ResponseTimeout) != "" {
		timeout = cfg.ResponseTimeout
	}
	d, err := time.ParseDuration(timeout)
	if err != nil || d <= 0 {
		d = 30 * time.Second
	}
	connectTimeout := config.DefaultGatewayConnectTimeout
	if strings.TrimSpace(cfg.ConnectTimeout) != "" {
		connectTimeout = cfg.ConnectTimeout
	}
	connect, err := time.ParseDuration(connectTimeout)
	if err != nil || connect <= 0 {
		connect = 2 * time.Second
	}
	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: connect, KeepAlive: 30 * time.Second}).DialContext,
		ResponseHeaderTimeout: d,
	}
	return &GatewayClient{baseURL: strings.TrimRight(cfg.URL, "/"), token: token, client: &http.Client{Transport: transport, Timeout: d}}, nil
}

func (c *GatewayClient) Search(ctx context.Context, query string, maxResults int) ([]Result, error) {
	var response protocol.SearchResponse
	if err := c.post(ctx, "/g1/search", protocol.SearchRequest{Version: protocol.Version, Query: query, MaxResults: maxResults}, &response); err != nil {
		return nil, err
	}
	results := make([]Result, 0, len(response.Results))
	for _, result := range response.Results {
		results = append(results, Result{Title: result.Title, URL: result.URL, Snippet: result.Snippet})
	}
	return results, nil
}

func (c *GatewayClient) FetchURL(ctx context.Context, rawURL string) (string, error) {
	var response protocol.FetchResponse
	if err := c.post(ctx, "/g1/fetch", protocol.FetchRequest{Version: protocol.Version, URL: rawURL}, &response); err != nil {
		return "", err
	}
	return response.Content, nil
}

func (c *GatewayClient) post(ctx context.Context, endpoint string, payload, target any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+endpoint, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set(protocol.VersionHeader, "1")
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("gateway %s unavailable; check gateway URL/network and retry: %w", strings.TrimPrefix(endpoint, "/g1/"), err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		var wire protocol.Error
		if json.Unmarshal(data, &wire) == nil && wire.Message != "" {
			return fmt.Errorf("gateway %s: %s", wire.Code, wire.Message)
		}
		return fmt.Errorf("gateway HTTP %d", resp.StatusCode)
	}
	dec := json.NewDecoder(io.LimitReader(resp.Body, config.DefaultGatewayMaxResponseBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(target); err != nil {
		return fmt.Errorf("decode gateway response: %w", err)
	}
	return nil
}

var _ Searcher = (*GatewayClient)(nil)
