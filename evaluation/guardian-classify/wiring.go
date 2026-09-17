package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/guardian"
	"github.com/samsaffron/term-llm/internal/typesafe"
)

// Keep setup aligned with cmd/guardian_wiring.go and cmd/classify.go without
// importing the shipping CLI or adding an evaluation surface to production.
type classifyClient = guardian.ClassifyClient

var newGuardianClassifyClient = func(opts typesafe.Options) (classifyClient, error) {
	return typesafe.NewClient(opts)
}

func newClassifyGuardianReview(cfg *config.Config, policy string) (func(context.Context, guardian.Request) (guardian.Decision, error), func(), error) {
	if err := cfg.Guardian.Classify.Validate(); err != nil {
		return nil, nil, err
	}
	provider, err := cfg.Classify.ResolveProvider(cfg.Guardian.Classify.Provider)
	if err != nil {
		return nil, nil, err
	}
	client, err := newTypeSafeClient(cfg)
	if err != nil {
		return nil, nil, fmt.Errorf("guardian classify provider: %w", err)
	}
	reviewer := &guardian.ClassifyReviewer{Client: client, Model: provider.Model, Policy: policy, MinConfidence: cfg.Guardian.Classify.MinConfidence}
	if cfg.Guardian.TimeoutSeconds > 0 {
		reviewer.Timeout = time.Duration(cfg.Guardian.TimeoutSeconds) * time.Second
	}
	return reviewer.Review, nil, nil
}

func newTypeSafeClient(cfg *config.Config) (classifyClient, error) {
	provider, err := cfg.Classify.ResolveProvider(cfg.Guardian.Classify.Provider)
	if err != nil {
		return nil, err
	}
	apiKey, err := provider.Key().Resolve()
	if err != nil {
		return nil, fmt.Errorf("resolve classify provider API key: %w", err)
	}
	baseURL, err := provider.BaseURLRef().Resolve()
	if err != nil {
		return nil, fmt.Errorf("resolve classify provider base URL: %w", err)
	}
	var timeout time.Duration
	if provider.TimeoutSeconds != 0 {
		if provider.TimeoutSeconds < 0 {
			return nil, errors.New("classify provider timeout_seconds must not be negative")
		}
		const maxTimeoutSeconds = int64(1<<63-1) / int64(time.Second)
		if int64(provider.TimeoutSeconds) > maxTimeoutSeconds {
			return nil, errors.New("classify provider timeout_seconds is too large")
		}
		timeout = time.Duration(provider.TimeoutSeconds) * time.Second
	}
	return newGuardianClassifyClient(typesafe.Options{APIKey: apiKey, BaseURL: baseURL, Timeout: timeout})
}
