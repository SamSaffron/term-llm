package core

// DeepCopyMap creates a deep copy of a map[string]interface{}
func DeepCopyMap(m map[string]interface{}) map[string]interface{} {
	if m == nil {
		return nil
	}
	result := make(map[string]interface{}, len(m))
	for k, v := range m {
		switch val := v.(type) {
		case map[string]interface{}:
			result[k] = DeepCopyMap(val)
		case []interface{}:
			result[k] = DeepCopySlice(val)
		default:
			result[k] = v
		}
	}
	return result
}

// DeepCopySlice recursively copies a JSON-like slice.
func DeepCopySlice(s []interface{}) []interface{} {
	if s == nil {
		return nil
	}
	result := make([]interface{}, len(s))
	for i, v := range s {
		switch val := v.(type) {
		case map[string]interface{}:
			result[i] = DeepCopyMap(val)
		case []interface{}:
			result[i] = DeepCopySlice(val)
		default:
			result[i] = v
		}
	}
	return result
}
