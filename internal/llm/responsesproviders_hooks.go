package llm

import (
	"github.com/samsaffron/term-llm/internal/llm/providers/responses"
	rp "github.com/samsaffron/term-llm/internal/llm/providers/responsesproviders"
)

// The Responses-dependent providers (OpenAI, ChatGPT, Copilot, Grok, OpenCode
// Zen and OpenCode Go) live in providers/responsesproviders; this installs the
// root-owned hooks.

func init() {
	rp.ParentHooks = rp.Hooks{
		StaticModelEntries: func(providerType string) []rp.StaticModelEntry {
			entries := ProviderModels[providerType]
			out := make([]rp.StaticModelEntry, 0, len(entries))
			for _, entry := range entries {
				out = append(out, rp.StaticModelEntry{
					ID:               entry.ID,
					InputLimit:       entry.InputLimit,
					OutputLimit:      entry.OutputLimit,
					ReasoningEfforts: entry.ReasoningEfforts,
				})
			}
			return out
		},
		BaseModelAndEffortForProvider:       BaseModelAndEffortForProvider,
		ConfigBaseModelAndEffortForProvider: configBaseModelAndEffortForProvider,
	}
}

func cloneFileUploadPolicy(policy *responses.FileUploadPolicy) *responses.FileUploadPolicy {
	if policy == nil {
		return nil
	}
	clone := *policy
	clone.NativeMimeTypes = cloneStrings(policy.NativeMimeTypes)
	clone.TextEmbedMimeTypes = cloneStrings(policy.TextEmbedMimeTypes)
	return &clone
}

var (
	grok46ReasoningEfforts = rp.Grok46ReasoningEfforts()
	chatGPTHTTPClient      = rp.ChatGPTHTTPClient()
)
