package live

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/samsaffron/term-llm/internal/config"
)

type sessionAudioOutput struct {
	Voice string `json:"voice,omitempty"`
}

type sessionAudio struct {
	Output sessionAudioOutput `json:"output"`
}

type sessionVoiceUpdate struct {
	Audio sessionAudio `json:"audio"`
}

type sessionDelegation struct {
	Type      string `json:"type"`
	AckFiller bool   `json:"ack_filler"`
}

type sessionInitialItem struct {
	Type    string        `json:"type"`
	Role    string        `json:"role"`
	Content []wireContent `json:"content"`
}

type sessionPayload struct {
	Model        string               `json:"model"`
	Instructions string               `json:"instructions,omitempty"`
	Audio        sessionAudio         `json:"audio"`
	Delegation   sessionDelegation    `json:"delegation"`
	InitialItems []sessionInitialItem `json:"initial_items,omitempty"`
}

func voiceUpdateJSON(voice string) (json.RawMessage, error) {
	payload := sessionVoiceUpdate{Audio: sessionAudio{Output: sessionAudioOutput{Voice: voice}}}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("encode live voice update: %w", err)
	}
	return encoded, nil
}

// SessionJSON builds the session object sent with a call creation request.
func SessionJSON(cfg config.LiveConfig, opts SessionOptions) (json.RawMessage, error) {
	if err := cfg.ChatGPT.ValidateVoice(); err != nil {
		return nil, err
	}
	model := strings.TrimSpace(cfg.ChatGPT.Model)
	if model == "" {
		model = config.DefaultLiveChatGPTModel
	}
	voice := strings.TrimSpace(cfg.ChatGPT.Voice)
	if voice == "" {
		voice = config.DefaultLiveChatGPTVoice
	}
	instructions := resolvedInstructions(cfg.Instructions, opts)
	payload := sessionPayload{
		Model:        model,
		Instructions: instructions,
		Audio:        sessionAudio{Output: sessionAudioOutput{Voice: voice}},
		Delegation:   sessionDelegation{Type: "client", AckFiller: true},
	}
	for _, item := range opts.InitialItems {
		text := strings.TrimSpace(item.Text)
		if text == "" {
			continue
		}
		role := strings.TrimSpace(item.Role)
		contentType := "input_text"
		switch role {
		case RoleAssistant:
			contentType = "output_text"
		case RoleUser, "developer":
		default:
			role = RoleUser
		}
		payload.InitialItems = append(payload.InitialItems, sessionInitialItem{
			Type:    "message",
			Role:    role,
			Content: []wireContent{{Type: contentType, Text: text}},
		})
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("encode live session: %w", err)
	}
	return encoded, nil
}
