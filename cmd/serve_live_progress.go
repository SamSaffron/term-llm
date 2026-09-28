package cmd

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/samsaffron/term-llm/internal/live"
)

// Live progress turns a delegated run's event stream into two kinds of note for
// the voice model, both rendered from event fields by fixed templates — never by
// a model, which would add latency and could invent progress:
//
//   - "[STATUS] ..." on the quiet channel: a snapshot of what is running, sent when
//     the state changes and at most every liveStatusMinSpacing. The voice model
//     keeps it and answers "what's it doing?" from it without speaking it unasked.
//   - "[PROGRESS] ..." on the speakable channel: a rare spoken note that fills a
//     silence, and an immediate one when the run needs the user's input.
//
// Both are marked Progress, so a transport that can only deliver a final result
// drops them rather than appending stale notes to that result.
const (
	liveProgressTick         = time.Second
	liveStatusMinSpacing     = 4 * time.Second
	liveStatusRefresh        = 15 * time.Second
	liveStartedAtSlack       = time.Minute
	liveProgressFirstSilence = 15 * time.Second
	liveProgressRepeatGap    = 30 * time.Second
	liveStatusMaxBytes       = 256
	liveToolInfoMaxBytes     = 80
	liveStatusPrefix         = "[STATUS] "
	liveProgressPrefix       = "[PROGRESS] "
)

type liveActiveTool struct {
	callID, name, info string
	started            time.Time
}

type liveFinishedTool struct {
	name, info string
	ok         bool
	duration   time.Duration
}

type liveWaiting struct {
	id, text string
	// announced is set once the user has been told. Announcing happens in tick,
	// not when the prompt is observed, so a replayed backlog that holds both a
	// prompt and its resolution never announces a request already answered.
	announced bool
}

// liveProgress tracks one delegated run. It is owned by the goroutine consuming
// the run's events and is not safe for concurrent use.
type liveProgress struct {
	emit func(live.DelegationChunk)
	now  func() time.Time

	began     time.Time
	active    []liveActiveTool
	done      int
	failed    int
	last      *liveFinishedTool
	ended     map[string]bool
	subagents map[string]string
	waiting   []liveWaiting

	dirty      bool
	statusAt   time.Time
	lastStatus string
	// spokeAt is when the voice model last had something to say: result text or
	// a spoken progress note. progressSpoken reports that the latest was a note,
	// which stretches the next gap so notes do not become a running commentary.
	spokeAt        time.Time
	progressSpoken bool
}

func newLiveProgress(emit func(live.DelegationChunk), now func() time.Time) *liveProgress {
	start := now()
	return &liveProgress{
		emit: emit, now: now, began: start, spokeAt: start,
		ended: make(map[string]bool), subagents: make(map[string]string),
	}
}

// observe folds one run event into the tracked state. It never emits: the
// caller ticks once a batch of events (such as a replayed backlog) is folded,
// so notes describe where the run is rather than where it passed through.
func (p *liveProgress) observe(event responseRunEvent) {
	switch event.Event {
	case "response.output_text.delta":
		var payload struct {
			Delta string `json:"delta"`
		}
		if json.Unmarshal(event.Data, &payload) != nil || payload.Delta == "" {
			return // nothing was said, so nothing moved on
		}
		p.spokeAt, p.progressSpoken = p.now(), false
		// The model is writing again, so nothing is still blocked on the user
		// even if a resolution event never reached this stream.
		p.clearWaiting()
	case "response.tool_exec.start":
		p.toolStarted(event.Data)
	case "response.tool_exec.end":
		p.toolEnded(event.Data)
	case "response.tool_exec.progress":
		p.subagentProgress(event.Data)
	case "response.approval.prompt":
		p.approvalRequested(event.Data)
	case "response.ask_user.prompt":
		p.questionAsked(event.Data)
	case "response.approval.resolved":
		p.inputResolved(event.Data, "approval_id")
	case "response.ask_user.resolved":
		p.inputResolved(event.Data, "call_id")
	}
}

// tick sends whatever is due: an input request not yet announced, else a spoken
// note after a silence, then a status snapshot if the state changed since the
// last one.
func (p *liveProgress) tick() {
	now := p.now()
	if !p.announceInput(now) && p.silenceDue(now) {
		p.speak(p.stillWorkingLine(now), now)
	}
	// A running tool's elapsed time goes stale without any event, so its
	// snapshot is refreshed, sparingly: every quiet note stays in the voice
	// model's context for the rest of the call.
	stale := len(p.active) > 0 && now.Sub(p.statusAt) >= liveStatusRefresh
	if !(p.dirty || stale) || now.Sub(p.statusAt) < liveStatusMinSpacing {
		return
	}
	p.dirty = false
	status := p.statusLine(now)
	if status == p.lastStatus {
		return
	}
	p.statusAt, p.lastStatus = now, status
	p.emit(live.DelegationChunk{Channel: live.ChannelQuiet, Text: status, Progress: true})
}

func (p *liveProgress) silenceDue(now time.Time) bool {
	if len(p.waiting) > 0 {
		// The user was already told what the run needs from them; repeating it
		// while they act on it is nagging, not progress.
		return false
	}
	gap := liveProgressFirstSilence
	if p.progressSpoken {
		gap = liveProgressRepeatGap
	}
	return now.Sub(p.spokeAt) >= gap
}

func (p *liveProgress) speak(text string, now time.Time) {
	p.spokeAt, p.progressSpoken = now, true
	p.emit(live.DelegationChunk{Channel: live.ChannelSpeakable, Text: text, Progress: true})
}

func (p *liveProgress) toolStarted(data []byte) {
	var payload struct {
		CallID    string `json:"call_id"`
		ToolName  string `json:"tool_name"`
		ToolInfo  string `json:"tool_info"`
		StartedAt int64  `json:"started_at"`
	}
	if json.Unmarshal(data, &payload) != nil || strings.TrimSpace(payload.ToolName) == "" {
		return
	}
	for _, tool := range p.active {
		if tool.callID == payload.CallID {
			return
		}
	}
	// The event's own start time survives a replay, so a tool that started just
	// before this tracker subscribed keeps its real age. Beyond a small margin
	// it is not trusted: a skewed or mis-scaled value would otherwise render as
	// a nonsense duration that crowds out the rest of the note.
	now := p.now()
	started := now
	if payload.StartedAt > 0 {
		started = time.UnixMilli(payload.StartedAt)
		if started.Before(p.began.Add(-liveStartedAtSlack)) || started.After(now) {
			started = now
		}
	}
	p.active = append(p.active, liveActiveTool{
		callID: payload.CallID, name: liveProgressText(payload.ToolName, liveToolInfoMaxBytes),
		info: liveProgressText(payload.ToolInfo, liveToolInfoMaxBytes), started: started,
	})
	p.dirty = true
}

func (p *liveProgress) toolEnded(data []byte) {
	var payload struct {
		CallID     string `json:"call_id"`
		ToolName   string `json:"tool_name"`
		ToolInfo   string `json:"tool_info"`
		Success    bool   `json:"success"`
		DurationMS int64  `json:"duration_ms"`
	}
	if json.Unmarshal(data, &payload) != nil {
		return
	}
	if payload.CallID != "" {
		if p.ended[payload.CallID] {
			return // a redelivered end must not count the tool twice
		}
		p.ended[payload.CallID] = true
		// An ask_user prompt is keyed by its tool call, so the tool ending is
		// also the end of that wait.
		p.resolveWaiting(payload.CallID)
	}
	finished := &liveFinishedTool{
		name: liveProgressText(payload.ToolName, liveToolInfoMaxBytes), info: liveProgressText(payload.ToolInfo, liveToolInfoMaxBytes),
		ok: payload.Success, duration: time.Duration(payload.DurationMS) * time.Millisecond,
	}
	for i, tool := range p.active {
		if tool.callID != payload.CallID {
			continue
		}
		if finished.info == "" {
			finished.info = tool.info
		}
		p.active = append(p.active[:i], p.active[i+1:]...)
		break
	}
	delete(p.subagents, payload.CallID)
	if finished.name == "" {
		return
	}
	p.done++
	if !finished.ok {
		p.failed++
	}
	p.last = finished
	p.dirty = true
}

// subagentProgress summarises the rate-limited progress events of spawned
// agents and jobs, which report counters rather than a transcript.
func (p *liveProgress) subagentProgress(data []byte) {
	var payload struct {
		CallID       string `json:"call_id"`
		State        string `json:"state"`
		CallsStarted int    `json:"calls_started"`
		CurrentTool  string `json:"current_tool"`
		Children     []struct {
			State string `json:"state"`
		} `json:"children"`
	}
	if json.Unmarshal(data, &payload) != nil || payload.CallID == "" {
		return
	}
	running := 0
	for _, child := range payload.Children {
		if child.State == "running" {
			running++
		}
	}
	summary := fmt.Sprintf("%d tool calls", payload.CallsStarted)
	if len(payload.Children) > 0 {
		summary = fmt.Sprintf("%d of %d agents active, %s", running, len(payload.Children), summary)
	} else if state := liveProgressText(payload.State, 20); state != "" {
		summary = state + ", " + summary
	}
	if current := liveProgressText(payload.CurrentTool, liveToolInfoMaxBytes); current != "" {
		summary += ", now " + current
	}
	if p.subagents[payload.CallID] != summary {
		p.subagents[payload.CallID] = summary
		p.dirty = true
	}
}

func (p *liveProgress) approvalRequested(data []byte) {
	var payload struct {
		ApprovalID string `json:"approval_id"`
		Title      string `json:"title"`
		Path       string `json:"path"`
	}
	if json.Unmarshal(data, &payload) != nil {
		return
	}
	subject := liveProgressText(payload.Title, liveToolInfoMaxBytes)
	if subject == "" {
		subject = liveProgressText(payload.Path, liveToolInfoMaxBytes)
	}
	text := "the user's approval"
	if subject != "" {
		text += " for " + quoteProgress(subject)
	}
	p.awaitInput(payload.ApprovalID, text)
}

func (p *liveProgress) questionAsked(data []byte) {
	var payload struct {
		CallID    string `json:"call_id"`
		Questions []struct {
			Header   string `json:"header"`
			Question string `json:"question"`
		} `json:"questions"`
	}
	if json.Unmarshal(data, &payload) != nil {
		return
	}
	text := "the user to answer a question"
	if len(payload.Questions) > 0 {
		question := payload.Questions[0].Question
		if strings.TrimSpace(question) == "" {
			question = payload.Questions[0].Header
		}
		if question = liveProgressText(question, 120); question != "" {
			text += ": " + quoteProgress(question)
		}
	}
	p.awaitInput(payload.CallID, text)
}

// awaitInput records a pause on the user. The next tick announces it at once:
// the run cannot continue until they act, so this is the one note that must
// not wait out a silence.
func (p *liveProgress) awaitInput(id, text string) {
	id = strings.TrimSpace(id)
	if id == "" {
		// Nothing could ever resolve an unnamed wait, so recording one would
		// silence the rest of the call.
		return
	}
	for _, waiting := range p.waiting {
		if waiting.id == id {
			return
		}
	}
	p.waiting = append(p.waiting, liveWaiting{id: id, text: text})
	p.dirty = true
}

// announceInput speaks every input request nobody has been told about, in one
// note, and reports whether it spoke.
func (p *liveProgress) announceInput(now time.Time) bool {
	var pending []string
	for i := range p.waiting {
		if !p.waiting[i].announced {
			pending = append(pending, p.waiting[i].text)
			p.waiting[i].announced = true
		}
	}
	if len(pending) == 0 {
		return false
	}
	p.speak(truncateProgress(liveProgressPrefix+waitingSentence(pending)+" in the app.", liveStatusMaxBytes), now)
	return true
}

// liveWaitingBudget bounds the whole waiting sentence, leaving room in a note
// for its prefix and the words around it.
const liveWaitingBudget = liveStatusMaxBytes - 64

// liveWaitingShown is how many requests a note names before counting the rest.
const liveWaitingShown = 3

// waitingSentence names pending requests within liveWaitingBudget. Each named
// request gets an equal share, so a long one cannot push another out, and any
// beyond liveWaitingShown are counted rather than dropped.
func waitingSentence(texts []string) string {
	const lead, separator = "Waiting for ", ", and for "
	shown := min(len(texts), liveWaitingShown)
	more := ""
	if extra := len(texts) - shown; extra > 0 {
		more = fmt.Sprintf(", and %d more", extra)
	}
	room := liveWaitingBudget - len(lead) - len(more) - len(separator)*max(shown-1, 0)
	share := room / max(shown, 1)
	bounded := make([]string, shown)
	for i := range bounded {
		bounded[i] = truncateProgress(texts[i], share)
	}
	return lead + strings.Join(bounded, separator) + more
}

func (p *liveProgress) waitingTexts() []string {
	texts := make([]string, len(p.waiting))
	for i, waiting := range p.waiting {
		texts[i] = waiting.text
	}
	return texts
}

func (p *liveProgress) inputResolved(data []byte, idField string) {
	var payload map[string]any
	if json.Unmarshal(data, &payload) != nil {
		return
	}
	id, _ := payload[idField].(string)
	p.resolveWaiting(id)
}

// resolveWaiting ends the wait with this id, if any.
func (p *liveProgress) resolveWaiting(id string) {
	id = strings.TrimSpace(id)
	for i, waiting := range p.waiting {
		if waiting.id == id {
			p.waiting = append(p.waiting[:i], p.waiting[i+1:]...)
			p.inputResumed()
			return
		}
	}
}

// clearWaiting ends every wait.
func (p *liveProgress) clearWaiting() {
	if len(p.waiting) > 0 {
		p.waiting = nil
		p.inputResumed()
	}
}

// inputResumed restarts the silence clock: the run resumes from here, so the
// next note is measured from now rather than from the prompt just answered.
func (p *liveProgress) inputResumed() {
	p.dirty = true
	p.spokeAt, p.progressSpoken = p.now(), false
}

// statusLine renders the quiet snapshot, most useful facts first because the
// line is cut to liveStatusMaxBytes.
func (p *liveProgress) statusLine(now time.Time) string {
	parts := make([]string, 0, 5)
	if len(p.waiting) > 0 {
		parts = append(parts, waitingSentence(p.waitingTexts()))
	}
	if len(p.waiting) == 0 || len(p.active) > 0 {
		parts = append(parts, p.activitySentence(now))
	}
	for _, tool := range p.active {
		if summary := p.subagents[tool.callID]; summary != "" {
			parts = append(parts, tool.name+": "+summary)
		}
	}
	if p.last != nil {
		parts = append(parts, "Last: "+describeFinishedTool(p.last))
	}
	parts = append(parts, p.totalsSentence(now))
	return truncateProgress(liveStatusPrefix+strings.Join(parts, ". ")+".", liveStatusMaxBytes)
}

// stillWorkingLine renders the spoken silence filler.
func (p *liveProgress) stillWorkingLine(now time.Time) string {
	line := liveProgressPrefix + "Still working, no result yet: " +
		lowerFirst(p.activitySentence(now)) + ". " + p.totalsSentence(now) + "."
	return truncateProgress(line, liveStatusMaxBytes)
}

func lowerFirst(text string) string {
	r, size := utf8.DecodeRuneInString(text)
	if size == 0 {
		return text
	}
	return string(unicode.ToLower(r)) + text[size:]
}

func (p *liveProgress) activitySentence(now time.Time) string {
	if len(p.active) == 0 {
		return "The agent is thinking"
	}
	tool := p.active[len(p.active)-1]
	sentence := "Running " + tool.name
	if tool.info != "" {
		sentence += " " + quoteProgress(tool.info)
	}
	sentence += " (" + formatProgressDuration(now.Sub(tool.started)) + ")"
	if others := len(p.active) - 1; others > 0 {
		sentence += fmt.Sprintf(" and %d more", others)
	}
	return sentence
}

func (p *liveProgress) totalsSentence(now time.Time) string {
	sentence := formatProgressDuration(now.Sub(p.began)) + " elapsed"
	if p.done == 0 {
		return sentence
	}
	noun := "tools"
	if p.done == 1 {
		noun = "tool"
	}
	sentence += fmt.Sprintf(", %d %s finished", p.done, noun)
	if p.failed > 0 {
		sentence += fmt.Sprintf(" (%d failed)", p.failed)
	}
	return sentence
}

func describeFinishedTool(tool *liveFinishedTool) string {
	text := tool.name
	if tool.info != "" {
		text += " " + quoteProgress(tool.info)
	}
	if tool.ok {
		return text + " ok in " + formatProgressDuration(tool.duration)
	}
	return text + " failed after " + formatProgressDuration(tool.duration)
}

func quoteProgress(text string) string { return "\"" + text + "\"" }

// liveSecretPatterns catch credentials a tool preview can carry, such as a
// shell command with an auth header or a key in a URL. Previews are sent to the
// voice provider, and the model may repeat them aloud. This is a conservative
// backstop, not a guarantee.
var liveSecretPatterns = []struct {
	pattern     *regexp.Regexp
	replacement string
}{
	// Whatever follows an Authorization or Cookie header name, quoted or not.
	{regexp.MustCompile(`(?i)\b(authorization["']?\s*[:=]\s*)(?:"[^"]*"|'[^']*'|(?:(?:basic|bearer|token)\s+)?[^\s"',;}]+)`), "${1}[redacted]"},
	{regexp.MustCompile(`(?i)\b((?:set-)?cookie["']?\s*:\s*)(?:"[^"]*"|'[^']*'|[^"'\n]+)`), "${1}[redacted]"},
	// curl's -b sends cookies. Only curl's: -b means something else elsewhere
	// (git checkout -b names a branch). --cookie is caught by the flag rule.
	{regexp.MustCompile(`(\bcurl\b[^|;&\n]*?\s-b\s+)(?:"[^"]*"|'[^']*'|[^\s"']+)`), "${1}[redacted]"},
	// curl's -u/--user carries user:password; elsewhere, only a --user value
	// shaped like user:password is treated as a credential.
	{regexp.MustCompile(`(\bcurl\b[^|;&\n]*?\s(?:-u|--user)(?:\s+|=))(?:"[^"]*"|'[^']*'|[^\s"']+)`), "${1}[redacted]"},
	{regexp.MustCompile(`(?i)((?:^|\s)--?user(?:name)?(?:\s+|=))["']?[^\s"':]+:[^\s"']+["']?`), "${1}[redacted]"},
	// user:password@ in URLs.
	{regexp.MustCompile(`://[^/\s:@]+:[^/\s@]+@`), "://[redacted]@"},
	// Well-known key prefixes, however short the rest.
	{regexp.MustCompile(`\b(?:sk|pk|rk)-[A-Za-z0-9_-]{8,}|\b(?:ghp|gho|ghu|ghs|ghr|github_pat)_[A-Za-z0-9_]{8,}|\bxox[abeoprs]-[A-Za-z0-9-]{8,}|\bglpat-[A-Za-z0-9_-]{8,}|\bAKIA[0-9A-Z]{12,}|\bAIza[0-9A-Za-z_-]{20,}`), "[redacted]"},
}

// liveSecretKey matches a field or flag name that may hold a secret;
// isLiveSecretKey then rejects the few names that only look like one.
const liveSecretKey = `[a-z0-9_.-]*(?:token|secret|passw(?:or)?d|pwd|api[_-]?key|access[_-]?key|private[_-]?key|auth|credential|cookie)[a-z0-9_.-]*`

// liveKeyedSecrets match a value named by its key: key=value, key: value and
// "key": "value", then --flag value. Group 1 is kept, group 2 is the key.
var liveKeyedSecrets = []*regexp.Regexp{
	regexp.MustCompile(`(?i)((["']?)\b(` + liveSecretKey + `)["']?\s*[:=]\s*)(?:"[^"]*"|'[^']*'|[^\s"'&,;}]+)`),
	regexp.MustCompile(`(?i)((^|\s)--?(` + liveSecretKey + `)\s+)(?:"[^"]*"|'[^']*'|[^\s"'-][^\s"']*)`),
}

var liveSecretKeyExact = regexp.MustCompile(`^` + liveSecretKey + `$`)

// isLiveSecretKey rejects names that match only through "author", such as
// git's --author or co-author, while --author-token still names a secret.
func isLiveSecretKey(key string) bool {
	key = strings.ToLower(key)
	if !strings.Contains(key, "authoriz") {
		key = strings.ReplaceAll(key, "author", "")
	}
	return liveSecretKeyExact.MatchString(key)
}

// liveBearer matches a bearer credential, quoted or bare. Group 1 is kept.
var liveBearer = regexp.MustCompile(`(?i)\b(bearer\s+["']?)([A-Za-z0-9._~+/=-]+)`)

// credentialShaped reports whether a bearer value looks like a credential
// rather than the next word of prose ("fix bearer handling"): it has a digit or
// symbol, mixed case after its first letter, or is long.
func credentialShaped(value string) bool {
	if len(value) >= 16 || strings.ContainsFunc(value, func(r rune) bool { return !unicode.IsLetter(r) }) {
		return true
	}
	_, size := utf8.DecodeRuneInString(value)
	rest := value[size:]
	return strings.ContainsFunc(rest, unicode.IsUpper) && strings.ContainsFunc(rest, unicode.IsLower)
}

// liveLongToken matches opaque runs long enough to be a credential. Slashes and
// dots are excluded so ordinary paths and file names survive.
var liveLongToken = regexp.MustCompile(`[A-Za-z0-9_+=-]{32,}`)

func redactLiveProgress(text string) string {
	for _, secret := range liveSecretPatterns {
		text = secret.pattern.ReplaceAllString(text, secret.replacement)
	}
	for _, keyed := range liveKeyedSecrets {
		text = keyed.ReplaceAllStringFunc(text, func(match string) string {
			parts := keyed.FindStringSubmatch(match)
			if !isLiveSecretKey(parts[3]) {
				return match
			}
			return parts[1] + "[redacted]"
		})
	}
	text = liveBearer.ReplaceAllStringFunc(text, func(match string) string {
		parts := liveBearer.FindStringSubmatch(match)
		if credentialShaped(parts[2]) {
			return parts[1] + "[redacted]"
		}
		return match
	})
	return liveLongToken.ReplaceAllStringFunc(text, func(token string) string {
		// Identifiers such as test names are long too; only a mix of letters and
		// digits looks like a key or hash.
		if strings.ContainsFunc(token, unicode.IsDigit) && strings.ContainsFunc(token, unicode.IsLetter) {
			return "[redacted]"
		}
		return token
	})
}

// liveProgressText reduces event text to one bounded line: credential-like
// tokens redacted, control characters dropped, whitespace collapsed, double
// quotes swapped so a quoted field keeps its boundary, and the result cut on a
// rune boundary with an ellipsis.
func liveProgressText(text string, limit int) string {
	text = redactLiveProgress(text)
	var b strings.Builder
	space := false
	for _, r := range text {
		switch {
		case unicode.IsSpace(r):
			space = b.Len() > 0
			continue
		case !unicode.IsPrint(r):
			continue
		case r == '"':
			r = '\''
		}
		if space {
			b.WriteByte(' ')
			space = false
		}
		b.WriteRune(r)
	}
	return truncateProgress(b.String(), limit)
}

// truncateProgress cuts text to at most limit bytes on a rune boundary, marking
// the cut with an ellipsis.
func truncateProgress(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	const ellipsis = "…"
	marker := ellipsis
	if limit < len(ellipsis) {
		marker = ""
	}
	cut := max(limit-len(marker), 0)
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return text[:cut] + marker
}

// formatProgressDuration renders a duration the way it would be said: tenths
// under ten seconds, whole seconds under a minute, then minutes and seconds.
func formatProgressDuration(d time.Duration) string {
	d = max(d, 0)
	switch {
	case d < 10*time.Second:
		return fmt.Sprintf("%.1fs", d.Seconds())
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d/time.Second))
	case d < time.Hour:
		return fmt.Sprintf("%dm%02ds", int(d/time.Minute), int(d%time.Minute/time.Second))
	default:
		return fmt.Sprintf("%dh%02dm", int(d/time.Hour), int(d%time.Hour/time.Minute))
	}
}
