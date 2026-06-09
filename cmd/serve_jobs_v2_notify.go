package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/tools"
)

const queuedAgentNotificationExcerptRunes = 240

func sanitizeJobsV2LLMNotifyOriginForRequest(r *http.Request, raw json.RawMessage) json.RawMessage {
	var cfg jobsV2LLMConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return raw
	}
	if !cfg.NotifyWhenDone && cfg.NotifyOrigin == nil {
		return raw
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return raw
	}
	delete(obj, "notify_origin")
	if cfg.NotifyWhenDone {
		if origin := trustedQueueAgentNotifyOriginFromRequest(r); origin != nil {
			if data, err := json.Marshal(origin); err == nil {
				obj["notify_origin"] = data
			}
		}
	}
	data, err := json.Marshal(obj)
	if err != nil {
		return raw
	}
	return data
}

func trustedQueueAgentNotifyOriginFromRequest(r *http.Request) *jobsV2NotifyOrigin {
	// Notify targets are trusted only from same-host runtime calls created by
	// queue_agent. Authenticated remote callers may request notify_when_done, but
	// they must not be able to choose an arbitrary session or Telegram chat.
	if r == nil || !isLoopbackRemoteAddr(r.RemoteAddr) {
		return nil
	}
	origin := strings.TrimSpace(r.Header.Get(tools.QueueAgentNotifyOriginHeader))
	switch origin {
	case tools.QueueAgentOriginWeb:
		sessionID := strings.TrimSpace(r.Header.Get(tools.QueueAgentNotifySessionIDHeader))
		if sessionID == "" {
			return nil
		}
		return &jobsV2NotifyOrigin{Origin: origin, SessionID: sessionID}
	case tools.QueueAgentOriginTelegram:
		chatID, err := strconv.ParseInt(strings.TrimSpace(r.Header.Get(tools.QueueAgentNotifyTelegramChatIDHeader)), 10, 64)
		if err != nil || chatID == 0 {
			return nil
		}
		return &jobsV2NotifyOrigin{Origin: origin, TelegramChatID: chatID}
	default:
		return nil
	}
}

func isLoopbackRemoteAddr(remoteAddr string) bool {
	host, _, err := net.SplitHostPort(strings.TrimSpace(remoteAddr))
	if err != nil {
		host = strings.TrimSpace(remoteAddr)
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (s *serveServer) notifyJobsV2RunDone(ctx context.Context, run jobsV2Run, job jobsV2Job, status jobsV2RunStatus, result jobsV2RunResult, exitReason string, truncated bool, errText string) error {
	_ = truncated
	if job.RunnerType != jobsV2RunnerLLM {
		return nil
	}
	var cfg jobsV2LLMConfig
	if err := json.Unmarshal(job.RunnerConfig, &cfg); err != nil {
		return nil
	}
	if !cfg.NotifyWhenDone {
		return nil
	}
	origin := cfg.NotifyOrigin
	if origin == nil {
		return nil
	}
	message := formatQueuedAgentDoneNotification(job.ID, cfg.AgentName, status, result, exitReason, errText)
	switch strings.TrimSpace(origin.Origin) {
	case tools.QueueAgentOriginWeb:
		return s.notifyQueuedAgentWeb(ctx, run.ID, origin.SessionID, message)
	case tools.QueueAgentOriginTelegram:
		return s.notifyQueuedAgentTelegram(ctx, origin.TelegramChatID, message)
	default:
		return nil
	}
}

func formatQueuedAgentDoneNotification(jobID, agentName string, status jobsV2RunStatus, result jobsV2RunResult, exitReason, errText string) string {
	jobID = strings.TrimSpace(jobID)
	if jobID == "" {
		jobID = "unknown"
	}
	agentName = strings.TrimSpace(agentName)
	if agentName == "" {
		agentName = "agent"
	}
	base := fmt.Sprintf("Queued job %s (%s) %s", jobID, agentName, strings.TrimSpace(string(status)))
	if detail := queuedAgentNotificationExcerpt(result, exitReason, errText); detail != "" {
		return base + ": " + detail
	}
	return base
}

func queuedAgentNotificationExcerpt(result jobsV2RunResult, exitReason, errText string) string {
	candidates := []string{errText}
	if strings.TrimSpace(exitReason) != "" && strings.TrimSpace(exitReason) != exitReasonNatural {
		candidates = append(candidates, exitReason)
	}
	candidates = append(candidates, result.Response, result.Stdout, result.Stderr, result.Thinking)
	for _, candidate := range candidates {
		if excerpt := compactNotificationExcerpt(candidate, queuedAgentNotificationExcerptRunes); excerpt != "" {
			return excerpt
		}
	}
	return ""
}

func compactNotificationExcerpt(text string, limit int) string {
	lines := strings.Split(strings.TrimSpace(text), "\n")
	kept := make([]string, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(strings.ToUpper(line), "STATUS:") {
			continue
		}
		kept = append(kept, line)
	}
	text = strings.Join(kept, " ")
	text = strings.Join(strings.Fields(text), " ")
	if limit <= 0 || utf8.RuneCountInString(text) <= limit {
		return text
	}
	runes := []rune(text)
	if limit <= 1 {
		return string(runes[:limit])
	}
	return strings.TrimSpace(string(runes[:limit-1])) + "…"
}

func (s *serveServer) notifyQueuedAgentWeb(ctx context.Context, runID, sessionID, message string) error {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return nil
	}
	if s != nil && s.sessionMgr != nil {
		if rt, ok := s.sessionMgr.Get(sessionID); ok && rt != nil {
			if rt.hasActiveRun() {
				// Job completion is delivered as an interjection only while the
				// originating session is already generating. Pass no classifier
				// provider so this best-effort notice cannot be upgraded into a
				// cancel/interrupt decision for the user's active run.
				if _, err := rt.InterruptMessage(ctx, llm.UserText(message), message, "job_notify_"+strings.TrimSpace(runID), nil); err == nil {
					return nil
				}
			}
			if err := rt.appendNotificationMessage(ctx, sessionID, message); err == nil {
				return nil
			}
		}
	}
	if s == nil {
		return nil
	}
	return appendQueuedAgentNotificationToStore(ctx, s.store, sessionID, message)
}

func (rt *serveRuntime) appendNotificationMessage(ctx context.Context, sessionID, message string) error {
	if rt == nil || strings.TrimSpace(message) == "" {
		return nil
	}
	if !rt.mu.TryLock() {
		return errServeSessionBusy
	}
	defer rt.mu.Unlock()

	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" && rt.sessionMeta != nil {
		sessionID = rt.sessionMeta.ID
	}
	msg := llm.AssistantText(message)
	rt.history = append(rt.history, msg)
	if rt.store == nil || sessionID == "" {
		return nil
	}
	dbCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := rt.store.AddMessage(dbCtx, sessionID, session.NewMessage(sessionID, msg, -1)); err != nil {
		return err
	}
	_ = rt.store.SetCurrent(dbCtx, sessionID)
	return nil
}

func appendQueuedAgentNotificationToStore(ctx context.Context, store session.Store, sessionID, message string) error {
	sessionID = strings.TrimSpace(sessionID)
	message = strings.TrimSpace(message)
	if store == nil || sessionID == "" || message == "" {
		return nil
	}
	dbCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if sess, err := store.Get(dbCtx, sessionID); err != nil || sess == nil {
		return nil
	}
	if err := store.AddMessage(dbCtx, sessionID, session.NewMessage(sessionID, llm.AssistantText(message), -1)); err != nil {
		return err
	}
	_ = store.SetCurrent(dbCtx, sessionID)
	return nil
}

func (s *serveServer) notifyQueuedAgentTelegram(ctx context.Context, chatID int64, message string) error {
	if s == nil || s.cfgRef == nil || chatID == 0 || strings.TrimSpace(message) == "" {
		return nil
	}
	token := strings.TrimSpace(s.cfgRef.Serve.Telegram.Token)
	if token == "" {
		return nil
	}
	if err := sendTelegramMessage(ctx, token, chatID, message, ""); err != nil {
		return err
	}
	logTelegramNotifySession(ctx, s.cfgRef, chatID, message, io.Discard)
	return nil
}
