package llm

// Current OpenAI API defaults. Limits and efforts are documented at
// https://developers.openai.com/api/docs/models/gpt-6.1-sol and /gpt-6-luna.
// Both use 1,050,000 total tokens minus 128,000 output = 922,000 input.
func currentOpenAIModelEntries() []ModelEntry {
	return []ModelEntry{
		{ID: "gpt-6.1-sol", InputLimit: 922_000, OutputLimit: 128_000, ReasoningEfforts: []string{"low", "medium", "high", "xhigh", "max"}},
		{ID: "gpt-6-luna", InputLimit: 922_000, OutputLimit: 128_000, ReasoningEfforts: []string{"none", "low", "medium", "high", "xhigh", "max"}},
	}
}
