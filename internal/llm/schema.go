package llm

func schemaRequired(schema map[string]interface{}) []string {
	if schema == nil {
		return nil
	}
	req, ok := schema["required"]
	if !ok {
		return nil
	}
	switch v := req.(type) {
	case []string:
		return v
	case []interface{}:
		out := make([]string, 0, len(v))
		for _, item := range v {
			if s, ok := item.(string); ok {
				out = append(out, s)
			}
		}
		return out
	default:
		return nil
	}
}
