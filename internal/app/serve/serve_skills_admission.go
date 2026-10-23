package serve

import (
	"fmt"
	"net/http"

	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/skills"
)

func (s *serveServer) startServeMainSkill(w http.ResponseWriter, r *http.Request, sess *session.Session, activation *skills.Activation, clientMessageID string) {
	if s.sessionMgr == nil {
		writeOpenAIError(w, http.StatusServiceUnavailable, "server_error", "session runtime is unavailable")
		return
	}
	manager := s.ensureResponseRuns()
	idempotencyKey := responseIdempotencyKeyFromRequest(r)
	if existing, ok := manager.getByIdempotencyKey(sess.ID, idempotencyKey); ok {
		writeServeMainSkillResponse(w, existing)
		return
	}
	if clientMessageID != "" && s.store != nil {
		committed, lookupErr := session.FindMessagesByClientMessageIDs(r.Context(), s.store, sess.ID, []string{clientMessageID})
		if lookupErr != nil {
			writeOpenAIError(w, http.StatusInternalServerError, "server_error", "lookup client_message_id: "+lookupErr.Error())
			return
		}
		if committed[clientMessageID] != nil {
			writeOpenAIError(w, http.StatusConflict, "client_message_already_committed", fmt.Sprintf("%s: %q", errResponseClientMessageAlreadyCommitted, clientMessageID))
			return
		}
	}
	if activeID := manager.activeRunID(sess.ID); activeID != "" {
		writeOpenAIError(w, http.StatusConflict, "conflict_error", "a response is already active for this session")
		return
	}
	runtime, _, err := s.runtimeForRequest(r.Context(), sess.ID)
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "server_error", err.Error())
		return
	}
	display := "/" + activation.Skill.Name
	if activation.RawArgs != "" {
		display += " " + activation.RawArgs
	}
	provenance := serveSkillProvenance(activation)
	provenance.Status = "running"
	instructions := skills.RenderActivationInstructions(activation)
	userMessage := core.UserText(display)
	userMessage.ClientMessageID = clientMessageID
	inputMessages := []core.Message{
		{Role: core.RoleDeveloper, Parts: []core.Part{{Type: core.PartSkillActivation, SkillActivation: provenance}, {Type: core.PartText, Text: instructions}}},
		userMessage,
	}
	run, err := s.startResponseRun(runtime, true, false, inputMessages, core.Request{
		SessionID:           sess.ID,
		Model:               runtime.defaultModel,
		AllowedTools:        append([]string(nil), activation.AllowedTools...),
		AllowedToolsPresent: activation.AllowedToolsPresent,
	}, sess.ID, startResponseRunOptions{
		previousResponseID: runtime.getLastResponseID(),
		notificationTurn:   &session.NotificationTurnAdmission{Human: true, Origin: s.notificationOrigin(r)},
		uiSession:          true,
		idempotencyKey:     idempotencyKey,
		runtimeSetup: func(req *core.Request) error {
			return setupServeMainSkillRequest(runtime, activation, req)
		},
	})
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "server_error", err.Error())
		return
	}
	writeServeMainSkillResponse(w, run)
}
