package llm

// vibeBinModels are the aliases the Mistral Vibe CLI offers a Vibe
// subscription. The account's catalog decides availability; vibe-bin reports
// the live list when a requested alias is not offered.
var vibeBinModels = []ModelEntry{
	{ID: "mistral-large-4-0-3"},
	{ID: "mistral-medium-3.5"},
	{ID: "glm-5-3"},
	{ID: "zai-glm-5-3-flash"},
}

func init() { ProviderModels["vibe-bin"] = vibeBinModels }
