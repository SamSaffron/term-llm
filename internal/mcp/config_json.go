package mcp

import "encoding/json"

// Keep extension fields intact when config writers add or remove other servers.
// Known fields are always encoded from the typed configuration, not stale JSON.
func decodeConfigObject(data []byte, target any, known any) (map[string]json.RawMessage, error) {
	if err := json.Unmarshal(data, target); err != nil {
		return nil, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(known)
	if err != nil {
		return nil, err
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &keys); err != nil {
		return nil, err
	}
	for key := range keys {
		delete(fields, key)
	}
	return fields, nil
}

func encodeConfigObject(known any, extra map[string]json.RawMessage) ([]byte, error) {
	data, err := json.Marshal(known)
	if err != nil || len(extra) == 0 {
		return data, err
	}
	fields := make(map[string]json.RawMessage, len(extra))
	for key, value := range extra {
		fields[key] = value
	}
	var typed map[string]json.RawMessage
	if err := json.Unmarshal(data, &typed); err != nil {
		return nil, err
	}
	for key, value := range typed {
		fields[key] = value
	}
	return json.Marshal(fields)
}

type configJSON Config
type serverConfigJSON ServerConfig

func (c *Config) UnmarshalJSON(data []byte) error {
	var value configJSON
	extra, err := decodeConfigObject(data, &value, configJSON{})
	if err != nil {
		return err
	}
	*c = Config(value)
	c.extra = extra
	return nil
}

func (c Config) MarshalJSON() ([]byte, error) {
	return encodeConfigObject(configJSON(c), c.extra)
}

func (c *ServerConfig) UnmarshalJSON(data []byte) error {
	var value serverConfigJSON
	// Include all known keys, even omitempty fields, when removing extensions.
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	var extra map[string]json.RawMessage
	if err := json.Unmarshal(data, &extra); err != nil {
		return err
	}
	for _, key := range []string{"type", "command", "args", "url", "headers", "oauth", "env", "always_load", "agents", "sampling"} {
		delete(extra, key)
	}
	*c = ServerConfig(value)
	c.extra = extra
	return c.validateAgents()
}

func (c ServerConfig) MarshalJSON() ([]byte, error) {
	return encodeConfigObject(serverConfigJSON(c), c.extra)
}
