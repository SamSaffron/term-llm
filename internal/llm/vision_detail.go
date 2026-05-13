package llm

func normalizeVisionDetail(detail string) string {
	switch detail {
	case "low", "high", "auto":
		return detail
	default:
		return ""
	}
}
