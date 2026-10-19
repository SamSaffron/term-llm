package sessions

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/ui"
	"github.com/spf13/cobra"
)

func (a App) List(cmd *cobra.Command, args []string) error {
	// Validate status if provided
	if a.Status != "" {
		validStatuses := []string{"active", "complete", "error", "interrupted"}
		if !slices.Contains(validStatuses, a.Status) {
			return fmt.Errorf("invalid status %q: must be one of %v", a.Status, validStatuses)
		}
	}

	// Validate mode if provided
	if a.Mode != "" {
		validModes := []string{"chat", "ask", "exec"}
		if !slices.Contains(validModes, a.Mode) {
			return fmt.Errorf("invalid mode %q: must be one of %v", a.Mode, validModes)
		}
	}

	store, err := a.openStore()
	if err != nil {
		return err
	}
	defer store.Close()

	ctx := context.Background()
	summaries, err := store.List(ctx, session.ListOptions{
		Provider: a.Provider,
		Mode:     session.SessionMode(a.Mode),
		Status:   session.SessionStatus(a.Status),
		Tag:      a.TagFilter,
		Limit:    a.Limit,
	})
	if err != nil {
		return fmt.Errorf("failed to list sessions: %w", err)
	}

	if len(summaries) == 0 {
		fmt.Println("No sessions found.")
		return nil
	}

	// Header line
	fmt.Printf("%4s %-25s %4s %5s %5s %-11s %-8s %s\n",
		"#", "SUMMARY", "MSGS", "TURNS", "TOOLS", "TOKENS", "STATUS", "AGE")
	fmt.Println(strings.Repeat("-", 100))

	for _, s := range summaries {
		summary := s.PreferredShortTitle()
		if summary == "" {
			summary = "(untitled)"
		}
		if len(summary) > 25 {
			summary = summary[:22] + "..."
		}

		// Format tokens as "total_input/output" in k format
		// Total input = input (after cache breakpoint) + cache read + cache write
		totalInput := s.InputTokens + s.CachedInputTokens + s.CacheWriteTokens
		tokens := formatSessionTokens(totalInput, s.OutputTokens)

		// Format status
		status := string(s.Status)
		if status == "" {
			status = "active"
		}

		age := FormatRelativeTime(s.UpdatedAt)

		// MSGS shows actual message count (MessageCount), TURNS shows LLM API round-trips
		fmt.Printf("%4d %-25s %4d %5d %5d %-11s %-8s %s\n",
			s.Number, summary, s.MessageCount, s.LLMTurns, s.ToolCalls, tokens, status, age)
	}

	return nil
}

// formatSessionTokens formats input/output tokens in compact form
func formatSessionTokens(input, output int) string {
	if input == 0 && output == 0 {
		return "-"
	}
	return fmt.Sprintf("%s/%s", ui.FormatTokenCount(input), ui.FormatTokenCount(output))
}
func (a App) Search(cmd *cobra.Command, args []string) error {
	store, err := a.openStore()
	if err != nil {
		return err
	}
	defer store.Close()

	query := strings.Join(args, " ")
	ctx := context.Background()
	results, err := store.Search(ctx, session.SearchOptions{Query: query, Limit: 20})
	if err != nil {
		return fmt.Errorf("search failed: %w", err)
	}

	if len(results) == 0 {
		fmt.Printf("No results found for '%s'\n", query)
		return nil
	}

	fmt.Printf("Found %d matches for '%s':\n\n", len(results), query)
	for _, r := range results {
		name := r.SessionName
		if name == "" {
			name = fmt.Sprintf("#%d", r.SessionNumber)
		}
		fmt.Printf("**%s** (%s)\n", name, r.Provider)
		fmt.Printf("  %s\n\n", r.Snippet)
	}

	return nil
}
func (a App) Show(cmd *cobra.Command, args []string) error {
	store, err := a.openStore()
	if err != nil {
		return err
	}
	defer store.Close()

	ctx := context.Background()
	sess, err := store.GetByPrefix(ctx, args[0])
	if err != nil {
		return fmt.Errorf("failed to get session: %w", err)
	}
	if sess == nil {
		return fmt.Errorf("session '%s' not found", args[0])
	}

	messages, err := store.GetMessages(ctx, sess.ID, 0, 0)
	if err != nil {
		return fmt.Errorf("failed to get messages: %w", err)
	}

	if a.JSON {
		data := struct {
			Session  *session.Session  `json:"session"`
			Messages []session.Message `json:"messages"`
		}{
			Session:  sess,
			Messages: messages,
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(data)
	}

	// Text output
	fmt.Printf("Session: %s\n", sess.ID)
	fmt.Printf("Title: %s\n", fallbackString(sess.PreferredShortTitle(), "(untitled)"))
	if sess.Name != "" {
		fmt.Printf("Name: %s\n", sess.Name)
	}
	if sess.GeneratedShortTitle != "" {
		fmt.Printf("Generated Short Title: %s\n", sess.GeneratedShortTitle)
	}
	if sess.GeneratedLongTitle != "" {
		fmt.Printf("Generated Long Title: %s\n", sess.GeneratedLongTitle)
	}
	fmt.Printf("Provider: %s\n", sess.Provider)
	fmt.Printf("Model: %s\n", sess.Model)
	mode := string(sess.Mode)
	if mode == "" {
		mode = "chat"
	}
	fmt.Printf("Mode: %s\n", mode)
	fmt.Printf("Created: %s\n", sess.CreatedAt.Format(time.RFC3339))
	fmt.Printf("Updated: %s\n", sess.UpdatedAt.Format(time.RFC3339))
	if sess.CWD != "" {
		fmt.Printf("CWD: %s\n", sess.CWD)
	}
	fmt.Printf("Messages: %d\n", len(messages))

	// Show metrics
	status := string(sess.Status)
	if status == "" {
		status = "active"
	}
	fmt.Printf("Status: %s\n", status)
	fmt.Printf("User Turns: %d\n", sess.UserTurns)
	fmt.Printf("LLM Turns: %d\n", sess.LLMTurns)
	fmt.Printf("Tool Calls: %d\n", sess.ToolCalls)
	totalInput := sess.InputTokens + sess.CachedInputTokens + sess.CacheWriteTokens
	fmt.Printf("Tokens: %s (input: %d, cache_read: %d, cache_write: %d, output: %d)\n",
		formatSessionTokens(totalInput, sess.OutputTokens),
		sess.InputTokens, sess.CachedInputTokens, sess.CacheWriteTokens, sess.OutputTokens)
	if sess.Tags != "" {
		fmt.Printf("Tags: %s\n", sess.Tags)
	}
	fmt.Println()

	for _, msg := range messages {
		role := string(msg.Role)
		if msg.Role == "user" {
			role = "❯"
		} else if msg.Role == "assistant" {
			role = "🤖"
		}
		content := msg.TextContent
		if len(content) > 200 {
			content = content[:197] + "..."
		}
		fmt.Printf("%s %s\n\n", role, content)
	}

	return nil
}

func (a App) Delete(cmd *cobra.Command, args []string) error {
	store, err := a.openStore()
	if err != nil {
		return err
	}
	defer store.Close()

	ctx := context.Background()
	// Look up session by prefix first to get full ID
	sess, err := store.GetByPrefix(ctx, args[0])
	if err != nil {
		return fmt.Errorf("failed to get session: %w", err)
	}
	if sess == nil {
		return fmt.Errorf("session '%s' not found", args[0])
	}

	if err := store.Delete(ctx, sess.ID); err != nil {
		return fmt.Errorf("failed to delete session: %w", err)
	}

	fmt.Printf("Deleted session: #%d\n", sess.Number)
	return nil
}

// FormatRelativeTime returns a human-readable relative time string
func FormatRelativeTime(t time.Time) string {
	dur := time.Since(t)
	switch {
	case dur < time.Minute:
		return "just now"
	case dur < time.Hour:
		return fmt.Sprintf("%dm ago", int(dur.Minutes()))
	case dur < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(dur.Hours()))
	case dur < 7*24*time.Hour:
		return fmt.Sprintf("%dd ago", int(dur.Hours()/24))
	default:
		return t.Format("Jan 2")
	}
}

func fallbackString(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}
func (a App) Name(cmd *cobra.Command, args []string) error {
	store, err := a.openStore()
	if err != nil {
		return err
	}
	defer store.Close()

	ctx := context.Background()
	sess, err := store.GetByPrefix(ctx, args[0])
	if err != nil {
		return fmt.Errorf("failed to get session: %w", err)
	}
	if sess == nil {
		return fmt.Errorf("session '%s' not found", args[0])
	}

	sess.Name = args[1]
	sess.TitleSource = session.TitleSourceUser
	if err := store.Update(ctx, sess); err != nil {
		return fmt.Errorf("failed to update session: %w", err)
	}

	fmt.Printf("Session #%d named: %s\n", sess.Number, sess.Name)
	return nil
}

func (a App) Tag(cmd *cobra.Command, args []string) error {
	store, err := a.openStore()
	if err != nil {
		return err
	}
	defer store.Close()

	ctx := context.Background()
	sess, err := store.GetByPrefix(ctx, args[0])
	if err != nil {
		return fmt.Errorf("failed to get session: %w", err)
	}
	if sess == nil {
		return fmt.Errorf("session '%s' not found", args[0])
	}

	// Parse existing tags
	existingTags := ParseTags(sess.Tags)

	// Add new tags (normalize and avoid duplicates)
	addedTags := []string{}
	for _, tag := range args[1:] {
		normalized := normalizeTag(tag)
		if normalized == "" {
			continue // Skip empty tags
		}
		if !containsTag(existingTags, normalized) {
			existingTags = append(existingTags, normalized)
			addedTags = append(addedTags, normalized)
		}
	}

	if len(addedTags) == 0 {
		fmt.Println("No new tags to add (all already present or empty).")
		return nil
	}

	sess.Tags = strings.Join(existingTags, ",")
	if err := store.Update(ctx, sess); err != nil {
		return fmt.Errorf("failed to update session: %w", err)
	}

	fmt.Printf("Added tags to session #%d: %s\n", sess.Number, strings.Join(addedTags, ", "))
	return nil
}

func (a App) Untag(cmd *cobra.Command, args []string) error {
	store, err := a.openStore()
	if err != nil {
		return err
	}
	defer store.Close()

	ctx := context.Background()
	sess, err := store.GetByPrefix(ctx, args[0])
	if err != nil {
		return fmt.Errorf("failed to get session: %w", err)
	}
	if sess == nil {
		return fmt.Errorf("session '%s' not found", args[0])
	}

	// Parse existing tags (already normalized)
	existingTags := ParseTags(sess.Tags)

	// Normalize tags to remove
	tagsToRemove := make([]string, 0, len(args[1:]))
	for _, t := range args[1:] {
		if nt := normalizeTag(t); nt != "" {
			tagsToRemove = append(tagsToRemove, nt)
		}
	}

	// Remove specified tags
	removedTags := []string{}
	newTags := []string{}
	for _, tag := range existingTags {
		if slices.Contains(tagsToRemove, tag) {
			removedTags = append(removedTags, tag)
		} else {
			newTags = append(newTags, tag)
		}
	}

	if len(removedTags) == 0 {
		fmt.Println("No tags to remove (none present).")
		return nil
	}

	sess.Tags = strings.Join(newTags, ",")
	if err := store.Update(ctx, sess); err != nil {
		return fmt.Errorf("failed to update session: %w", err)
	}

	fmt.Printf("Removed tags from session #%d: %s\n", sess.Number, strings.Join(removedTags, ", "))
	return nil
}

// ParseTags parses a comma-separated tag string into a slice of normalized tags.
func ParseTags(tags string) []string {
	if tags == "" {
		return nil
	}
	parts := strings.Split(tags, ",")
	var result []string
	for _, tag := range parts {
		normalized := normalizeTag(tag)
		if normalized != "" {
			result = append(result, normalized)
		}
	}
	return result
}

// normalizeTag normalizes a tag: trim whitespace, lowercase.
func normalizeTag(tag string) string {
	return strings.ToLower(strings.TrimSpace(tag))
}

// containsTag checks if a slice contains a tag (already normalized)
func containsTag(tags []string, tag string) bool {
	return slices.Contains(tags, normalizeTag(tag))
}
