package share

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/samsaffron/term-llm/internal/agents/gist"
)

type githubGistClient interface {
	Create(description string, public bool, files map[string]string) (*gist.Gist, error)
	Get(gistID string) (*gist.Gist, error)
	Update(gistID string, files map[string]string) error
}

type GitHubPublisher struct {
	newClient  func() (githubGistClient, error)
	http       *http.Client
	sleep      func(context.Context, time.Duration) bool
	previewURL func(string) string
}

func NewGitHubPublisher() *GitHubPublisher {
	return &GitHubPublisher{
		newClient:  func() (githubGistClient, error) { return gist.NewClient() },
		http:       &http.Client{Timeout: 2 * time.Second},
		sleep:      sleepContext,
		previewURL: gist.PreviewURL,
	}
}

func (p *GitHubPublisher) Capabilities(context.Context) (Capabilities, error) {
	return Capabilities{
		Protocol: Protocol,
		Version:  Version,
		Provider: Provider{
			ID:   ProviderGitHub,
			Name: "GitHub Gist",
			Help: "Requires the gh CLI authenticated to github.com (run: gh auth login --hostname github.com).",
		},
		Operations:        []Operation{OperationCreate, OperationUpdate},
		Visibilities:      []Visibility{VisibilityUnlisted, VisibilityPublic},
		DefaultVisibility: VisibilityUnlisted,
		Notes: []string{
			"Unlisted Gists are accessible to anyone with the link; they are not private.",
			"Shares are created on the active GitHub account selected by gh.",
		},
	}, nil
}

func (p *GitHubPublisher) Create(ctx context.Context, req Request) (Result, error) {
	if err := ValidateRequest(req); err != nil {
		return Result{}, errorWithDiagnostic(ErrorProtocol, "share request is invalid", err.Error(), err)
	}
	if req.Visibility != VisibilityUnlisted && req.Visibility != VisibilityPublic {
		return Result{}, NewError(ErrorUnsupportedVisibility, "GitHub Gist supports unlisted and public visibility")
	}
	client, err := p.newClient()
	if err != nil {
		return Result{}, githubClientError(err)
	}
	files := make(map[string]string, len(req.Files))
	for _, file := range req.Files {
		files[file.Name] = string(file.Content)
	}
	created, err := client.Create(req.Description, req.Visibility == VisibilityPublic, files)
	if err != nil {
		return Result{}, githubClientError(err)
	}
	if created == nil {
		return Result{}, NewError(ErrorProvider, "GitHub returned an empty share result")
	}
	sourceURL := strings.TrimSpace(created.URL)
	if sourceURL == "" {
		sourceURL = gist.GetURL(created.ID)
	}
	result := Result{
		Provider: ProviderGitHub, ID: created.ID, URL: p.previewURL(created.ID), SourceURL: sourceURL,
		Visibility: req.Visibility,
	}
	if err := ValidateResult(result); err != nil {
		return Result{}, errorWithDiagnostic(ErrorProtocol, "GitHub returned an invalid share result", err.Error(), err)
	}
	result.Ready = p.probeReady(ctx, result.URL)
	return result, nil
}

func (p *GitHubPublisher) Update(ctx context.Context, id string, req Request) (Result, error) {
	if gist.PreviewURL(id) == "" {
		return Result{}, NewError(ErrorProtocol, "stored GitHub share ID is invalid")
	}
	if err := ValidateRequest(req); err != nil {
		return Result{}, errorWithDiagnostic(ErrorProtocol, "share request is invalid", err.Error(), err)
	}
	if req.Visibility != VisibilityUnlisted && req.Visibility != VisibilityPublic {
		return Result{}, NewError(ErrorUnsupportedVisibility, "GitHub Gist supports unlisted and public visibility")
	}
	client, err := p.newClient()
	if err != nil {
		return Result{}, githubClientError(err)
	}
	existing, err := client.Get(id)
	if err != nil {
		return Result{}, githubClientError(err)
	}
	if existing == nil {
		return Result{}, NewError(ErrorProvider, "GitHub returned an empty share result")
	}
	files := make(map[string]string, len(req.Files))
	for _, file := range req.Files {
		files[file.Name] = string(file.Content)
	}
	if err := client.Update(id, files); err != nil {
		return Result{}, githubClientError(err)
	}
	visibility := VisibilityUnlisted
	if existing.Public {
		visibility = VisibilityPublic
	}
	sourceURL := strings.TrimSpace(existing.URL)
	if sourceURL == "" {
		sourceURL = gist.GetURL(id)
	}
	result := Result{
		Provider: ProviderGitHub, ID: id, URL: p.previewURL(id), SourceURL: sourceURL,
		Visibility: visibility,
	}
	if err := ValidateResult(result); err != nil {
		return Result{}, errorWithDiagnostic(ErrorProtocol, "GitHub returned an invalid share result", err.Error(), err)
	}
	result.Ready = p.probeReady(ctx, result.URL)
	return result, nil
}

func githubClientError(err error) error {
	switch {
	case errors.Is(err, gist.ErrDependencyMissing):
		return errorWithDiagnostic(ErrorDependencyMissing, "GitHub sharing requires the gh CLI; install it from https://cli.github.com", err.Error(), err)
	case errors.Is(err, gist.ErrAuthRequired):
		return errorWithDiagnostic(ErrorAuthRequired, "GitHub authentication is required; run gh auth login --hostname github.com", err.Error(), err)
	default:
		return errorWithDiagnostic(ErrorProvider, "GitHub could not create or update the share", err.Error(), err)
	}
}

func (p *GitHubPublisher) probeReady(parent context.Context, target string) bool {
	ctx, cancel := context.WithTimeout(parent, 10*time.Second)
	defer cancel()
	backoff := 200 * time.Millisecond
	for attempt := 0; attempt < 5; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
		if err != nil {
			return false
		}
		req.Header.Set("User-Agent", "term-llm-share-readiness/1")
		resp, err := p.http.Do(req)
		if err == nil {
			_ = resp.Body.Close()
			switch {
			case resp.StatusCode >= 200 && resp.StatusCode < 300:
				return true
			case resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests:
				return false
			}
		}
		if attempt == 4 || !p.sleep(ctx, backoff) {
			return false
		}
		backoff *= 2
	}
	return false
}

func sleepContext(ctx context.Context, duration time.Duration) bool {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
