package cmd

import (
	"context"
	"github.com/samsaffron/term-llm/internal/skills"
	"sync"
	"time"
)

type serveSkillRunEvent struct {
	Sequence int       `json:"sequence"`
	Type     string    `json:"type"`
	Data     any       `json:"data,omitempty"`
	At       time.Time `json:"at"`
}

const (
	serveSkillRunEventHistoryLimit = 1024
	serveSkillsCacheTTL            = 5 * time.Second
)

type serveSkillsCacheEntry struct {
	setup      *skills.Setup
	discovered time.Time
}

type serveSkillRun struct {
	mu                sync.Mutex
	ID                string
	SessionID         string
	ChildSessionID    string
	ClientMessageID   string
	SkillName         string
	Agent             string
	Status            string
	Output            string
	StartedAt         time.Time
	CompletedAt       time.Time
	Activation        *skills.Activation
	cancel            context.CancelFunc
	events            []serveSkillRunEvent
	eventHead         int
	nextSequence      int
	subscribers       map[int]chan serveSkillRunEvent
	subscriberDropped map[int]bool
	nextSubscriber    int
	cleanupTimer      *time.Timer
}
