package engine

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"github.com/samsaffron/term-llm/internal/llm/core"
	"mime"
	"os"
	"path/filepath"
	"strings"

	"github.com/samsaffron/term-llm/internal/appdata"
	"github.com/samsaffron/term-llm/internal/runtimeoutput"
)

// applyToolOutputTruncation applies global and compaction truncation limits
// to all textual tool output, including structured content parts. Global limit
// fires first (typically stricter), then compaction limit as a safety net.
func (e *Engine) applyToolOutputTruncation(output core.ToolOutput) core.ToolOutput {
	e.callbackMu.RLock()
	maxChars := e.maxToolOutputChars
	cc := e.compactionConfig
	e.callbackMu.RUnlock()

	if maxChars > 0 {
		output = core.TruncateToolOutput(output, maxChars)
	}
	if cc != nil && cc.MaxToolResultChars > 0 {
		output = core.TruncateToolOutput(output, cc.MaxToolResultChars)
	}
	return output
}

// getCompactionCallback returns the current compaction callback under read lock.
func (e *Engine) getCompactionCallback() CompactionCallback {
	e.callbackMu.RLock()
	cb := e.onCompaction
	e.callbackMu.RUnlock()
	return cb
}

// estimatedTokens returns the estimated input token count for the next API
// call. Uses total_tokens (input+output) from the last API response as the exact
// baseline through the last assistant turn, then adds heuristic estimates only
// for messages structurally appended after that assistant turn.
func (e *Engine) estimatedTokens(messages []core.Message) int {
	return e.EstimateTokens(messages)
}

// nonSystemMessages returns all messages that are not system messages.
func nonSystemMessages(messages []core.Message) []core.Message {
	var result []core.Message
	for _, msg := range messages {
		if msg.Role != core.RoleSystem {
			result = append(result, msg)
		}
	}
	return result
}

// IsToolAllowed checks if a tool can be executed under current restrictions.
func (e *Engine) IsToolAllowed(name string) bool {
	e.allowedMu.RLock()
	defer e.allowedMu.RUnlock()
	return e.allowedTools == nil || e.allowedTools[name]
}

const indirectVisionInstruction = "Uploaded images are represented as local file-path references for this text-only model. When visual content matters, call the view_image tool with the referenced file_path and, if useful, a focused question. Do not claim to have inspected an image unless you have called view_image or the user-provided text is sufficient."

func (e *Engine) prepareProviderRequest(req core.Request) core.Request {
	prepared := req
	prepared.Messages = providerSafeRequestMessages(req.Messages)
	if e.IndirectVision() {
		normalized := ensureIndirectVisionImagePaths(prepared.Messages)
		rewritten, changed := rewriteImagePartsAsReferences(normalized)
		prepared.Messages = rewritten
		if changed {
			prepared.Messages = prependIndirectVisionInstruction(prepared.Messages)
		}
		return prepared
	}
	prepared.Messages = hydrateImageDataFromPaths(prepared.Messages)
	return prepared
}

func providerSafeRequestMessages(messages []core.Message) []core.Message {
	var output []core.Message
	for index, message := range messages {
		needsRewrite := false
		for _, part := range message.Parts {
			if part.Type == core.PartDisplayText || part.Type == core.PartConversationStart || part.Type == core.PartPlatformContext || part.Type == core.PartSkillActivation || part.Type == core.PartAgentMention || part.Type == core.PartDiffComment || part.Type == core.PartGoalSteering {
				needsRewrite = true
				break
			}
		}
		if !needsRewrite {
			if output != nil {
				output = append(output, message)
			}
			continue
		}
		if output == nil {
			output = append([]core.Message(nil), messages[:index]...)
		}
		copyMessage := message
		copyMessage.Parts = make([]core.Part, 0, len(message.Parts))
		for _, part := range message.Parts {
			switch part.Type {
			case core.PartDisplayText, core.PartConversationStart, core.PartPlatformContext, core.PartSkillActivation, core.PartDiffComment, core.PartGoalSteering:
				continue
			case core.PartAgentMention:
				part.Type = core.PartText
			}
			copyMessage.Parts = append(copyMessage.Parts, part)
		}
		if len(copyMessage.Parts) > 0 {
			output = append(output, copyMessage)
		}
	}
	if output == nil {
		return messages
	}
	return output
}

func ensureIndirectVisionImagePaths(messages []core.Message) []core.Message {
	out := make([]core.Message, len(messages))
	copy(out, messages)
	for i, msg := range messages {
		if msg.Role != core.RoleUser {
			continue
		}
		parts := make([]core.Part, len(msg.Parts))
		copy(parts, msg.Parts)
		changed := false
		for j, part := range parts {
			if part.Type != core.PartImage || isTermLLMUploadPath(part.ImagePath) {
				continue
			}
			path, ok := saveImageDataToUploads(part.ImageData)
			if !ok {
				continue
			}
			parts[j].ImagePath = path
			changed = true
		}
		if changed {
			out[i].Parts = parts
		}
	}
	return out
}

func saveImageDataToUploads(imageData *core.ToolImageData) (string, bool) {
	if imageData == nil || strings.TrimSpace(imageData.Base64) == "" {
		return "", false
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(imageData.Base64))
	if err != nil || len(raw) == 0 {
		return "", false
	}
	dataDir, err := appdata.GetDataDir()
	if err != nil {
		return "", false
	}
	uploadsDir := filepath.Join(dataDir, "uploads")
	if err := os.MkdirAll(uploadsDir, 0o700); err != nil {
		return "", false
	}
	ext := imageExtensionForMediaType(imageData.MediaType)
	sum := sha256.Sum256(raw)
	path := filepath.Join(uploadsDir, fmt.Sprintf("uploaded_image_%x%s", sum[:16], ext))
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		if _, statErr := os.Stat(path); statErr == nil {
			return path, true
		}
		return "", false
	}
	return path, true
}

func imageExtensionForMediaType(mediaType string) string {
	switch strings.ToLower(strings.TrimSpace(mediaType)) {
	case "image/jpeg":
		return ".jpg"
	case "image/png":
		return ".png"
	case "image/gif":
		return ".gif"
	case "image/webp":
		return ".webp"
	default:
		return ".png"
	}
}

func hydrateImageDataFromPaths(messages []core.Message) []core.Message {
	out := make([]core.Message, len(messages))
	copy(out, messages)
	for i, msg := range messages {
		parts := make([]core.Part, len(msg.Parts))
		copy(parts, msg.Parts)
		changed := false
		for j, part := range parts {
			if part.Type != core.PartImage || strings.TrimSpace(part.ImagePath) == "" {
				continue
			}
			if part.ImageData != nil && strings.TrimSpace(part.ImageData.Base64) != "" {
				continue
			}
			data, ok := readHydratableImagePath(part.ImagePath)
			if !ok {
				parts[j] = core.Part{Type: core.PartText, Text: unavailableImageText(part.ImagePath)}
				changed = true
				continue
			}
			mediaType := ""
			if part.ImageData != nil {
				mediaType = strings.TrimSpace(part.ImageData.MediaType)
			}
			if mediaType == "" {
				mediaType = strings.TrimSpace(mime.TypeByExtension(strings.ToLower(filepath.Ext(part.ImagePath))))
			}
			if mediaType == "" {
				mediaType = "image/png"
			}
			imageData := core.ToolImageData{MediaType: mediaType, Base64: base64.StdEncoding.EncodeToString(data)}
			if part.ImageData != nil {
				imageData.Detail = part.ImageData.Detail
			}
			parts[j].ImageData = &imageData
			changed = true
		}
		if changed {
			out[i].Parts = parts
		}
	}
	return out
}

func readHydratableImagePath(path string) ([]byte, bool) {
	path = strings.TrimSpace(path)
	if path == "" || !isTermLLMUploadPath(path) {
		return nil, false
	}
	data, err := os.ReadFile(path)
	if err != nil || len(data) == 0 {
		return nil, false
	}
	return data, true
}

func isTermLLMUploadPath(path string) bool {
	dataDir, err := appdata.GetDataDir()
	if err != nil {
		return false
	}
	uploadsDir := filepath.Join(dataDir, "uploads")
	uploadsDir, err = filepath.EvalSymlinks(uploadsDir)
	if err != nil {
		return false
	}
	path, err = filepath.EvalSymlinks(strings.TrimSpace(path))
	if err != nil {
		return false
	}
	if path == uploadsDir {
		return false
	}
	return strings.HasPrefix(path, uploadsDir+string(filepath.Separator))
}

func unavailableImageText(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return "[image unavailable: saved file could not be read]"
	}
	return fmt.Sprintf("[image unavailable: saved file could not be read at %s]", path)
}

func prependIndirectVisionInstruction(messages []core.Message) []core.Message {
	for _, msg := range messages {
		if msg.Role == core.RoleDeveloper && strings.Contains(core.CollectTextParts(msg.Parts), "Uploaded images are represented as local file-path references") {
			return messages
		}
	}
	instruction := core.Message{Role: core.RoleDeveloper, Parts: []core.Part{{Type: core.PartText, Text: indirectVisionInstruction}}}
	insertAt := 0
	for insertAt < len(messages) && messages[insertAt].Role == core.RoleSystem {
		insertAt++
	}
	out := make([]core.Message, 0, len(messages)+1)
	out = append(out, messages[:insertAt]...)
	out = append(out, instruction)
	out = append(out, messages[insertAt:]...)
	return out
}

func rewriteImagePartsAsReferences(messages []core.Message) ([]core.Message, bool) {
	out := make([]core.Message, len(messages))
	changedAny := false
	for i, msg := range messages {
		out[i] = msg
		if msg.Role != core.RoleUser {
			continue
		}
		parts := make([]core.Part, 0, len(msg.Parts))
		changed := false
		for _, part := range msg.Parts {
			if part.Type != core.PartImage {
				parts = append(parts, part)
				continue
			}
			changed = true
			changedAny = true
			parts = append(parts, core.Part{Type: core.PartText, Text: imageReferenceText(part)})
		}
		if changed {
			out[i].Parts = parts
		}
	}
	return out, changedAny
}

func imageReferenceText(part core.Part) string {
	path := strings.TrimSpace(part.ImagePath)
	mediaType := ""
	if part.ImageData != nil {
		mediaType = strings.TrimSpace(part.ImageData.MediaType)
	}
	if path == "" {
		if mediaType == "" {
			return "[User uploaded an image, but no local file path is available for view_image.]"
		}
		return fmt.Sprintf("[User uploaded an image (%s), but no local file path is available for view_image.]", mediaType)
	}
	if mediaType == "" {
		return fmt.Sprintf("[User uploaded image: %s — use view_image with this file_path to inspect it.]", path)
	}
	return fmt.Sprintf("[User uploaded image: %s (%s) — use view_image with this file_path to inspect it.]", path, mediaType)
}

// PrepareCompactionContext lets available request-context tools attach
// request-only restoration state after durable compaction is generated. Callers
// use this for manual compaction paths; automatic engine compaction calls it
// with the already-filtered request specs.
func (e *Engine) PrepareCompactionContext(ctx context.Context, sessionID string, specs []core.ToolSpec, result *CompactionResult) error {
	if e == nil || e.provider == nil || result == nil || !e.provider.Capabilities().ToolCalls {
		return nil
	}
	specs = e.FilterAllowedToolSpecs(specs)
	for _, spec := range specs {
		tool, ok := e.tools.Get(spec.Name)
		if !ok {
			continue
		}
		contextTool, ok := tool.(RequestContextTool)
		if !ok {
			continue
		}
		if err := contextTool.PrepareCompactionContext(ctx, sessionID, result); err != nil {
			return fmt.Errorf("restore %s context after compaction: %w", spec.Name, err)
		}
	}
	return nil
}

func (e *Engine) prepareRequestContext(ctx context.Context, req *core.Request) {
	if e == nil || req == nil {
		return
	}
	for _, spec := range req.Tools {
		tool, ok := e.tools.Get(spec.Name)
		if !ok {
			continue
		}
		contextTool, ok := tool.(RequestContextTool)
		if !ok {
			continue
		}
		messages, err := contextTool.PrepareRequestContext(ctx, req.SessionID, req.Messages)
		if err != nil {
			// Restored tool state is an optional context enhancement. A stale or
			// temporarily unavailable state store must not prevent the request.
			runtimeoutput.Warn("request context restoration failed; continuing without it", "tool", spec.Name, "error", err)
			continue
		}
		req.Messages = messages
	}
}
