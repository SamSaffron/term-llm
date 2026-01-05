package search

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"golang.org/x/net/html"
)

// Result is a single search result.
type Result struct {
	Title   string
	URL     string
	Snippet string
}

// Searcher performs web searches.
type Searcher interface {
	Search(ctx context.Context, query string, maxResults int) ([]Result, error)
}

// DuckDuckGoLite implements Searcher using the DuckDuckGo lite HTML page.
type DuckDuckGoLite struct {
	client  *http.Client
	baseURL string
}

func NewDuckDuckGoLite(client *http.Client) *DuckDuckGoLite {
	if client == nil {
		client = http.DefaultClient
	}
	return &DuckDuckGoLite{
		client:  client,
		baseURL: "https://lite.duckduckgo.com/lite/",
	}
}

func (d *DuckDuckGoLite) Search(ctx context.Context, query string, maxResults int) ([]Result, error) {
	if strings.TrimSpace(query) == "" {
		return nil, fmt.Errorf("empty query")
	}

	params := url.Values{}
	params.Set("q", query)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.baseURL+"?"+params.Encode(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "term-llm")

	resp, err := d.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("duckduckgo http %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	return ParseDuckDuckGoLiteHTML(string(body), maxResults)
}

// ParseDuckDuckGoLiteHTML parses results from DuckDuckGo lite HTML.
func ParseDuckDuckGoLiteHTML(htmlText string, maxResults int) ([]Result, error) {
	doc, err := html.Parse(strings.NewReader(htmlText))
	if err != nil {
		return nil, err
	}

	var results []Result
	var lastResult *Result

	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && n.Data == "a" {
			class := getAttr(n, "class")
			if strings.Contains(class, "result-link") {
				href := getAttr(n, "href")
				title := strings.TrimSpace(textContent(n))
				if href != "" && title != "" {
					url := normalizeDuckDuckGoURL(href)
					results = append(results, Result{Title: title, URL: url})
					lastResult = &results[len(results)-1]
				}
			}
		}

		if n.Type == html.ElementNode && n.Data == "span" {
			class := getAttr(n, "class")
			if strings.Contains(class, "result-snippet") && lastResult != nil && lastResult.Snippet == "" {
				lastResult.Snippet = strings.TrimSpace(textContent(n))
			}
		}

		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}

	walk(doc)

	if maxResults <= 0 || maxResults > len(results) {
		maxResults = len(results)
	}
	return results[:maxResults], nil
}

func getAttr(n *html.Node, key string) string {
	for _, attr := range n.Attr {
		if attr.Key == key {
			return attr.Val
		}
	}
	return ""
}

func textContent(n *html.Node) string {
	var b strings.Builder
	var walk func(*html.Node)
	walk = func(node *html.Node) {
		if node.Type == html.TextNode {
			b.WriteString(node.Data)
		}
		for c := node.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return b.String()
}

func normalizeDuckDuckGoURL(raw string) string {
	if !strings.HasPrefix(raw, "http") {
		return raw
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	if parsed.Host != "duckduckgo.com" || !strings.HasPrefix(parsed.Path, "/l/") {
		return raw
	}
	params := parsed.Query()
	uddg := params.Get("uddg")
	if uddg == "" {
		return raw
	}
	decoded, err := url.QueryUnescape(uddg)
	if err != nil {
		return raw
	}
	return decoded
}
