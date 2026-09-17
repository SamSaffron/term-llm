package cmd

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/tools"
)

const (
	// liveNewSessionMaxNames bounds the candidates an error names. The voice model
	// reads that message aloud, so a full project or agent list is not an answer.
	liveNewSessionMaxNames = 12
	// liveNewSessionMaxCandidates bounds an ambiguous project match: five is
	// already more than the one spoken sentence that follows can weigh.
	liveNewSessionMaxCandidates = 5
)

// liveNewSessionForTool adapts the shared creation path to the tool package's
// transport-free types.
func (s *serveServer) liveNewSessionForTool(record *liveSession) func(context.Context, tools.LiveNewSessionRequest) (tools.LiveNewSessionResult, error) {
	return func(ctx context.Context, request tools.LiveNewSessionRequest) (tools.LiveNewSessionResult, error) {
		return s.startLiveNewSession(ctx, record, request)
	}
}

// startLiveNewSession creates a brand-new conversation and moves the call to it.
//
// Order is the contract. Everything that can be refused is resolved first, then
// the conversation is created through the same path the browser uses, and only
// then is the call bound — through switchLiveSession, the one switch path, so
// live.session_changed, the host note, and the browser's follow-along are not
// reimplemented here. There is still no chat turn: the request that asked for a
// new conversation was answered by the router, and the conversation it created
// starts empty.
func (s *serveServer) startLiveNewSession(ctx context.Context, record *liveSession, request tools.LiveNewSessionRequest) (tools.LiveNewSessionResult, error) {
	if record == nil {
		return tools.LiveNewSessionResult{}, errLiveCallEnded
	}
	bound := strings.TrimSpace(record.boundSession())
	if bound == "" {
		return tools.LiveNewSessionResult{}, errors.New("the live call is not bound to a chat session")
	}
	agent, err := s.resolveLiveNewSessionAgent(ctx, bound, request.Agent)
	if err != nil {
		return tools.LiveNewSessionResult{}, err
	}
	projectID, err := s.resolveLiveNewSessionProject(ctx, bound, request.Project)
	if err != nil {
		return tools.LiveNewSessionResult{}, err
	}
	create := createWebSessionRequest{Agent: agent}
	if s.projectsEnabled {
		// The same choice the browser makes for a blank conversation: the selected
		// project, or the explicit no-project workspace when there is none.
		if projectID == "" {
			create.NoProject = true
		} else {
			create.ProjectID = projectID
		}
	} else {
		// With project mode off the registry is not consulted at all, exactly as the
		// browser's composer does not offer one.
		create.UseDefaultWorkspace = true
	}
	sess, err := s.createWebSession(ctx, create)
	if err != nil {
		return tools.LiveNewSessionResult{}, fmt.Errorf("could not start the new conversation: %w", err)
	}
	target, err := s.switchLiveSession(ctx, record, sess.ID)
	if err != nil {
		// The conversation is durable and the user will see it in the sidebar, so
		// it is named rather than discarded: the call stayed where it was, and
		// asking again is a switch to this id.
		return tools.LiveNewSessionResult{}, fmt.Errorf("session #%d (%s) was created but the call did not move to it: %w — it can still be reached with live_switch_session", sess.Number, sess.ID, err)
	}
	name := strings.TrimSpace(sess.Agent)
	if name == "" {
		// The row is what the sidebar shows, so it is what is reported; a factory
		// that records no agent leaves the resolved name as the honest answer.
		name = agent
	}
	return tools.LiveNewSessionResult{
		SessionID: target.SessionID, SessionNumber: target.Number,
		Project: target.Project, Agent: name,
	}, nil
}

// resolveLiveNewSessionAgent decides which agent the new conversation runs. A
// server that pins an agent runs every conversation with it, an omitted request
// inherits the conversation the user is already in, and a named one is matched
// against the agent registry case-insensitively and answered with the registry's
// own spelling. An unknown name is refused with the available ones: a
// conversation started with an agent that does not exist would be a broken row.
func (s *serveServer) resolveLiveNewSessionAgent(ctx context.Context, boundSessionID, requested string) (string, error) {
	requested = strings.TrimSpace(requested)
	if pinned := strings.TrimSpace(s.cfg.agentName); pinned != "" {
		if requested != "" && !strings.EqualFold(requested, pinned) {
			return "", fmt.Errorf("this server runs every conversation with the %q agent, so it cannot start one with %q", pinned, requested)
		}
		return pinned, nil
	}
	if requested == "" {
		if s.store == nil || boundSessionID == "" {
			return "", nil
		}
		sess, err := s.store.Get(ctx, boundSessionID)
		if err != nil || sess == nil {
			// An unreadable row leaves the new conversation with the server default
			// rather than failing the request: this is an inherited convenience, and
			// a broken read of it is not a reason to refuse a conversation the user
			// asked for.
			return "", nil
		}
		// Inherited as-is rather than re-validated: the name was written by the
		// host when the bound conversation was created, and an agent that has since
		// been removed must not make "start a new conversation" fail.
		return strings.TrimSpace(sess.Agent), nil
	}
	// The list the browser's agent picker was given at startup, sorted so the
	// message reads the same way twice.
	names := make([]string, 0, len(s.cfg.agentNames))
	for _, name := range s.cfg.agentNames {
		if trimmed := strings.TrimSpace(name); trimmed != "" {
			names = append(names, trimmed)
		}
	}
	slices.Sort(names)
	for _, name := range names {
		if strings.EqualFold(name, requested) {
			return name, nil
		}
	}
	if len(names) == 0 {
		return "", fmt.Errorf("this server has no agents configured, so it cannot start a conversation with %q", requested)
	}
	return "", fmt.Errorf("there is no %q agent; available agents are %s", requested, strings.Join(names[:min(len(names), liveNewSessionMaxNames)], ", "))
}

// resolveLiveNewSessionProject decides which project the new conversation lands
// in. An omitted project stays where the user is; a named one is matched against
// the durable registry — exact id, then exact name, then a unique
// case-insensitive substring — and anything ambiguous or unknown is refused with
// the candidates, never guessed, because the project decides which files the
// conversation can touch.
func (s *serveServer) resolveLiveNewSessionProject(ctx context.Context, boundSessionID, requested string) (string, error) {
	requested = strings.TrimSpace(requested)
	if requested == "" {
		if s.store == nil || boundSessionID == "" {
			return "", nil
		}
		sess, err := s.store.Get(ctx, boundSessionID)
		if err != nil || sess == nil {
			return "", nil
		}
		return strings.TrimSpace(sess.ProjectID), nil
	}
	projects, ok := s.projectStore()
	if !ok {
		return "", errors.New("projects are unavailable on this server, so one cannot be chosen")
	}
	// Archived projects are absent by default and could not start a conversation
	// anyway, so they are not candidates and are not offered as alternatives.
	all, err := projects.ListProjects(ctx, session.ProjectListOptions{})
	if err != nil {
		return "", fmt.Errorf("list projects: %w", err)
	}
	var (
		byID     *session.Project
		byName   *session.Project
		partials []*session.Project
	)
	for i := range all {
		project := &all[i]
		name := strings.TrimSpace(project.Name)
		switch {
		case project.ID == requested:
			byID = project
		case strings.EqualFold(name, requested):
			byName = project
		case strings.Contains(strings.ToLower(name), strings.ToLower(requested)):
			partials = append(partials, project)
		}
	}
	if byID != nil {
		return byID.ID, nil
	}
	if byName != nil {
		return byName.ID, nil
	}
	if len(partials) == 1 {
		return partials[0].ID, nil
	}
	if len(partials) > 1 {
		return "", fmt.Errorf("%q matches more than one project (%s); name the project exactly", requested, strings.Join(boundedProjectLabels(partials, liveNewSessionMaxCandidates), ", "))
	}
	// Available projects only: a project whose directory is missing is one the
	// host refuses to bind a second later, so offering it would send the user
	// round the same loop.
	available := make([]*session.Project, 0, len(all))
	for i := range all {
		if cheapProjectStatus(all[i]).Available {
			available = append(available, &all[i])
		}
	}
	if len(available) == 0 {
		return "", fmt.Errorf("there is no %q project and this server has no projects available", requested)
	}
	return "", fmt.Errorf("there is no %q project; available projects are %s", requested, strings.Join(boundedProjectLabels(available, liveNewSessionMaxNames), ", "))
}

// boundedProjectLabels renders projects as the voice model has to hear them to
// act again — the name it can say, and the id that is unambiguous when two
// projects share a name — sorted so the same question reads the same way twice,
// and cut to a count a spoken answer can carry.
func boundedProjectLabels(projects []*session.Project, limit int) []string {
	labels := make([]string, 0, len(projects))
	for _, project := range projects {
		labels = append(labels, fmt.Sprintf("%s (%s)", strings.TrimSpace(project.Name), project.ID))
	}
	slices.Sort(labels)
	return labels[:min(len(labels), limit)]
}
