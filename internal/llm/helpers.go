package llm

import "github.com/samsaffron/term-llm/internal/llm/core"

func chooseModel(requested, fallback string) string { return core.ChooseModel(requested, fallback) }

func truncate(s string, maxLen int) string { return core.Truncate(s, maxLen) }
