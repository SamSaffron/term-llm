package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	sessionsapp "github.com/samsaffron/term-llm/internal/app/sessions"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/session"
	sharepkg "github.com/samsaffron/term-llm/internal/share"
	"github.com/spf13/cobra"
)

type sessionsSharePublisherMock struct {
	createCalls int
	updateCalls int
	lastRequest sharepkg.Request
	help        string
	notes       []string
}

func (m *sessionsSharePublisherMock) Capabilities(context.Context) (sharepkg.Capabilities, error) {
	return sharepkg.Capabilities{
		Protocol: sharepkg.Protocol, Version: sharepkg.Version,
		Provider:     sharepkg.Provider{ID: "acme", Name: "Acme Vault", Help: m.help},
		Operations:   []sharepkg.Operation{sharepkg.OperationCreate, sharepkg.OperationUpdate},
		Visibilities: []sharepkg.Visibility{sharepkg.VisibilityPrivate}, DefaultVisibility: sharepkg.VisibilityPrivate,
		Notes: append([]string(nil), m.notes...),
	}, nil
}

func (m *sessionsSharePublisherMock) Create(_ context.Context, req sharepkg.Request) (sharepkg.Result, error) {
	m.createCalls++
	m.lastRequest = req
	return sharepkg.Result{Provider: "acme", ID: "opaque", URL: "https://share.example/opaque", Visibility: sharepkg.VisibilityPrivate, Ready: true}, nil
}

func (m *sessionsSharePublisherMock) Update(_ context.Context, _ string, req sharepkg.Request) (sharepkg.Result, error) {
	m.updateCalls++
	m.lastRequest = req
	return sharepkg.Result{Provider: "acme", ID: "opaque", URL: "https://share.example/opaque", Visibility: sharepkg.VisibilityPrivate, Ready: true}, nil
}

func TestSessionsShareCreatesThenUpdatesAndPersistsWholeSession(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	oldDBPath, oldNoSession := sessionDBPath, noSession
	oldVisibility, oldNew, oldJSON, oldRaw := sessionsShareVisibility, sessionsShareNew, sessionsShareJSON, sessionsShareIncludeRawReasoning
	oldFactory := newSessionSharePublisher
	t.Cleanup(func() {
		sessionDBPath, noSession = oldDBPath, oldNoSession
		sessionsShareVisibility, sessionsShareNew, sessionsShareJSON, sessionsShareIncludeRawReasoning = oldVisibility, oldNew, oldJSON, oldRaw
		newSessionSharePublisher = oldFactory
	})
	sessionDBPath, noSession = filepath.Join(t.TempDir(), "sessions.db"), false
	sessionsShareVisibility, sessionsShareNew, sessionsShareJSON, sessionsShareIncludeRawReasoning = "private", false, true, false

	store, err := session.NewStore(session.Config{Enabled: true, Path: sessionDBPath})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	sess := &session.Session{ID: "session-share-cli", Name: "CLI share", Provider: "mock", Model: "mock", Mode: session.ModeChat, CreatedAt: now, UpdatedAt: now}
	if err := store.Create(context.Background(), sess); err != nil {
		t.Fatal(err)
	}
	if err := store.AddMessage(context.Background(), sess.ID, session.NewMessage(sess.ID, llm.UserText("hello"), -1)); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	publisher := &sessionsSharePublisherMock{}
	newSessionSharePublisher = func(config.ShareConfig) (sharepkg.Publisher, error) { return publisher, nil }
	run := func() sessionsapp.ShareOutput {
		t.Helper()
		var output bytes.Buffer
		command := &cobra.Command{}
		command.SetOut(&output)
		if err := runSessionsShare(command, []string{sess.ID}); err != nil {
			t.Fatal(err)
		}
		var decoded sessionsapp.ShareOutput
		if err := json.Unmarshal(output.Bytes(), &decoded); err != nil {
			t.Fatalf("decode output %q: %v", output.String(), err)
		}
		return decoded
	}
	first := run()
	if first.Updated || publisher.createCalls != 1 || publisher.updateCalls != 0 || first.Scope != session.ShareScopeSession {
		t.Fatalf("first=%+v calls=%d/%d", first, publisher.createCalls, publisher.updateCalls)
	}

	check, err := session.NewStore(session.Config{Enabled: true, Path: sessionDBPath})
	if err != nil {
		t.Fatal(err)
	}
	persisted, err := check.Get(context.Background(), sess.ID)
	_ = check.Close()
	if err != nil || persisted.Share == nil || persisted.Share.Provider != "acme" || persisted.Share.Scope != session.ShareScopeSession {
		t.Fatalf("persisted share=%+v error=%v", persisted.Share, err)
	}

	second := run()
	if !second.Updated || publisher.createCalls != 1 || publisher.updateCalls != 1 {
		t.Fatalf("second=%+v calls=%d/%d", second, publisher.createCalls, publisher.updateCalls)
	}

	sessionsShareNew = true
	var output, warnings bytes.Buffer
	command := &cobra.Command{}
	command.SetOut(&output)
	command.SetErr(&warnings)
	if err := runSessionsShare(command, []string{sess.ID}); err != nil {
		t.Fatal(err)
	}
	if publisher.createCalls != 2 || !strings.Contains(warnings.String(), "replace the saved share state") || !strings.Contains(warnings.String(), "may remain active") {
		t.Fatalf("new share warning=%q createCalls=%d", warnings.String(), publisher.createCalls)
	}

	publisher.notes = []string{"Private links expire."}
	publisher.help = "Run acme login."
	sessionsShareNew, sessionsShareJSON = false, false
	output.Reset()
	command.SetOut(&output)
	if err := runSessionsShare(command, []string{sess.ID}); err != nil {
		t.Fatal(err)
	}
	text := output.String()
	if !strings.Contains(text, "Note: Private links expire.") || !strings.Contains(text, "Help: Run acme login.") || strings.Index(text, "Note:") > strings.Index(text, "Updated share") {
		t.Fatalf("provider guidance was not shown before publishing: %q", text)
	}
}

func TestSessionsShareCommandRegistered(t *testing.T) {
	for _, command := range sessionsCmd.Commands() {
		if command.Name() == "share" {
			if command.Flag("visibility") == nil || command.Flag("new") == nil || command.Flag("json") == nil || command.Flag("include-raw-reasoning") == nil {
				t.Fatalf("share flags incomplete")
			}
			return
		}
	}
	t.Fatal("sessions share command is not registered")
}
