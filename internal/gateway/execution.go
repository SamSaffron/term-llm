package gateway

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/samsaffron/term-llm/internal/gateway/protocol"
	"github.com/samsaffron/term-llm/internal/llm"
)

// startInference is the single provider-edge admission and setup path shared by
// the private /g1 transport and the public OpenAI-compatible Responses edge.
// It applies catalog, policy, concurrency, credential, retry, state, filesystem
// isolation, and cancellation rules without instantiating an agent runtime.
func (s *Server) startInference(parent context.Context, client Client, envelope protocol.InferenceRequest, providerReq llm.Request, rejectInlineToolLoop bool) (*inferenceExecution, *inferenceRequestError) {
	if providerReq.Model == "" {
		if pc := s.currentConfig().GetProviderConfig(envelope.Provider); pc != nil {
			providerReq.Model = pc.Model
		}
	}

	releaseInference, ok := s.acquireInference(client)
	if !ok {
		s.recordFailure(client, envelope, providerReq, "client_concurrency_limited", time.Now().UTC())
		return nil, &inferenceRequestError{
			Status: http.StatusTooManyRequests,
			Code:   "client_concurrency_limited",
			Message: "this gateway client has reached its concurrent inference limit; " +
				"wait for another request to finish",
		}
	}
	fail := func(status int, code, message string) (*inferenceExecution, *inferenceRequestError) {
		releaseInference()
		return nil, &inferenceRequestError{Status: status, Code: code, Message: message}
	}

	entry, found, catalogErr := s.currentCatalogProvider(parent, envelope.Provider)
	if catalogErr != nil {
		slog.Error("refresh gateway provider catalog for inference", "request_id", envelope.RequestID, "provider", envelope.Provider, "error", catalogErr)
		return fail(http.StatusServiceUnavailable, "catalog_unavailable", "gateway provider catalog is temporarily unavailable; retry or contact the gateway operator")
	}
	if !found {
		s.recordFailure(client, envelope, providerReq, "unknown_provider", time.Now().UTC())
		return fail(http.StatusNotFound, "unknown_provider", "provider is not available")
	}
	if !catalogEntryAllowsModel(entry, envelope.Provider, providerReq.Model) {
		s.recordFailure(client, envelope, providerReq, "unknown_model", time.Now().UTC())
		return fail(http.StatusNotFound, "unknown_model", "model is not available for this gateway provider; choose a catalog model or ask the gateway operator to allow unlisted models")
	}
	if !s.cfg.Policy.Allows(envelope.Provider, providerReq.Model, entry.CLI) || !client.Policy.Allows(envelope.Provider, providerReq.Model, entry.CLI) {
		s.recordFailure(client, envelope, providerReq, "policy_denied", time.Now().UTC())
		return fail(http.StatusForbidden, "policy_denied", "provider/model is denied by gateway policy; choose an allowed model or contact the gateway operator")
	}
	if rejectInlineToolLoop && len(providerReq.Tools) > 0 && entry.CLI && entry.Capabilities.InlineToolLoop {
		s.recordFailure(client, envelope, providerReq, "incompatible_tool_request", time.Now().UTC())
		return fail(http.StatusBadRequest, "incompatible_tool_request", "this CLI provider requires an inline tool loop; Responses function tools are rejected because the gateway never executes client tools")
	}

	started := time.Now().UTC()
	failStarted := func(status int, code, message string) (*inferenceExecution, *inferenceRequestError) {
		s.recordUsage(client, envelope, providerReq, llm.Usage{}, code, started)
		return fail(status, code, message)
	}
	if err := nonInteractiveAuthReady(entry.Type); err != nil {
		status, code := classifyProviderError(err, entry.Type)
		slog.Error("gateway provider authentication unavailable", "request_id", envelope.RequestID, "provider", envelope.Provider, "error", err)
		return failStarted(status, code, safeProviderErrorMessage(code, envelope.Provider))
	}
	provider, err := s.cfg.ProviderFactory(s.centralConfig(), envelope.Provider, providerReq.Model)
	if err != nil {
		status, code := classifyProviderError(err, entry.Type)
		slog.Error("create gateway provider", "request_id", envelope.RequestID, "provider", envelope.Provider, "error", err)
		return failStarted(status, code, safeProviderErrorMessage(code, envelope.Provider))
	}
	provider = llm.WrapWithRetry(provider, llm.RetryConfig{
		MaxAttempts:    s.cfg.UpstreamRetryAttempts,
		MaxElapsedTime: s.cfg.UpstreamRetryMaxElapsed,
		BaseBackoff:    time.Second,
		MaxBackoff:     5 * time.Second,
	})
	if envelope.State != "" {
		plain, openErr := s.cfg.Sealer.Open(envelope.State, client.ID, envelope.Provider)
		if openErr != nil {
			return failStarted(http.StatusBadRequest, "invalid_state", "provider state is invalid or does not belong to this client/provider")
		}
		importer, ok := provider.(llm.ProviderStateImporter)
		if !ok {
			return failStarted(http.StatusBadRequest, "invalid_state", "provider does not accept state")
		}
		if err := importer.ImportProviderState(plain); err != nil {
			return failStarted(http.StatusBadRequest, "invalid_state", "provider state was rejected")
		}
	}

	tempDir, err := s.newRunTempDir()
	if err != nil {
		return failStarted(http.StatusInternalServerError, "internal", "could not create isolated provider directory")
	}
	providerReq.WorkingDir = tempDir
	ctx, cancel := context.WithCancel(parent)
	stream, err := provider.Stream(ctx, providerReq)
	if err != nil {
		cancel()
		_ = os.RemoveAll(tempDir)
		status, code := classifyProviderError(err, entry.Type)
		slog.Error("gateway provider request failed", "request_id", envelope.RequestID, "provider", envelope.Provider, "error", err)
		return failStarted(status, code, safeProviderErrorMessage(code, envelope.Provider))
	}

	return &inferenceExecution{
		server: s, client: client, envelope: envelope, request: providerReq, entry: entry,
		provider: provider, stream: stream, ctx: ctx, cancel: cancel, release: releaseInference,
		tempDir: tempDir, started: started,
	}, nil
}

func (s *Server) logProviderStreamError(execution *inferenceExecution, err error) (int, string) {
	status, code := classifyProviderError(err, execution.entry.Type)
	if errors.Is(err, context.Canceled) || errors.Is(execution.ctx.Err(), context.Canceled) {
		code = "canceled"
	}
	slog.Error("gateway provider stream failed", "request_id", execution.envelope.RequestID, "provider", execution.envelope.Provider, "error", err)
	return status, code
}
