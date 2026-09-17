package live

import (
	"encoding/json"
	"fmt"
	"strings"
)

const geminiDelegationTool = "delegate_to_controller"

type geminiSetupMessage struct {
	Setup geminiSetup `json:"setup"`
}

type geminiSetup struct {
	Model                    string                    `json:"model"`
	GenerationConfig         geminiGenerationConfig    `json:"generationConfig"`
	SystemInstruction        geminiContent             `json:"systemInstruction"`
	Tools                    []geminiTool              `json:"tools"`
	InputAudioTranscription  map[string]any            `json:"inputAudioTranscription"`
	OutputAudioTranscription map[string]any            `json:"outputAudioTranscription"`
	HistoryConfig            *geminiHistoryConfig      `json:"historyConfig,omitempty"`
	RealtimeInputConfig      geminiRealtimeInputConfig `json:"realtimeInputConfig"`
}

type geminiGenerationConfig struct {
	ResponseModalities []string           `json:"responseModalities"`
	SpeechConfig       geminiSpeechConfig `json:"speechConfig"`
}

type geminiSpeechConfig struct {
	VoiceConfig geminiVoiceConfig `json:"voiceConfig"`
}

type geminiVoiceConfig struct {
	PrebuiltVoiceConfig geminiPrebuiltVoiceConfig `json:"prebuiltVoiceConfig"`
}

type geminiPrebuiltVoiceConfig struct {
	VoiceName string `json:"voiceName"`
}

type geminiHistoryConfig struct {
	InitialHistoryInClientContent bool `json:"initialHistoryInClientContent"`
}

type geminiRealtimeInputConfig struct {
	ActivityHandling string `json:"activityHandling"`
}

type geminiTool struct {
	FunctionDeclarations []geminiFunctionDeclaration `json:"functionDeclarations"`
}

type geminiFunctionDeclaration struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

type geminiContent struct {
	Role  string       `json:"role,omitempty"`
	Parts []geminiPart `json:"parts"`
}

type geminiPart struct {
	Text       string            `json:"text,omitempty"`
	InlineData *geminiInlineData `json:"inlineData,omitempty"`
}

type geminiInlineData struct {
	MIMEType string `json:"mimeType"`
	Data     []byte `json:"data"`
}

type geminiClientMessage struct {
	ClientContent *geminiClientContent `json:"clientContent,omitempty"`
	RealtimeInput *geminiRealtimeInput `json:"realtimeInput,omitempty"`
	ToolResponse  *geminiToolResponse  `json:"toolResponse,omitempty"`
}

type geminiClientContent struct {
	Turns        []geminiContent `json:"turns"`
	TurnComplete bool            `json:"turnComplete"`
}

type geminiRealtimeInput struct {
	Audio          *geminiInlineData `json:"audio,omitempty"`
	Text           string            `json:"text,omitempty"`
	AudioStreamEnd bool              `json:"audioStreamEnd,omitempty"`
}

type geminiToolResponse struct {
	FunctionResponses []geminiFunctionResponse `json:"functionResponses"`
}

type geminiFunctionResponse struct {
	ID       string         `json:"id"`
	Name     string         `json:"name"`
	Response map[string]any `json:"response"`
}

type geminiServerMessage struct {
	SetupComplete        *struct{}                   `json:"setupComplete,omitempty"`
	ServerContent        *geminiServerContent        `json:"serverContent,omitempty"`
	ToolCall             *geminiToolCall             `json:"toolCall,omitempty"`
	ToolCallCancellation *geminiToolCallCancellation `json:"toolCallCancellation,omitempty"`
	Error                *geminiWireError            `json:"error,omitempty"`
	GoAway               *struct {
		TimeLeft string `json:"timeLeft"`
	} `json:"goAway,omitempty"`
}

type geminiServerContent struct {
	ModelTurn                 *geminiContent       `json:"modelTurn,omitempty"`
	InputTranscription        *geminiTranscription `json:"inputTranscription,omitempty"`
	InterimInputTranscription *geminiTranscription `json:"interimInputTranscription,omitempty"`
	OutputTranscription       *geminiTranscription `json:"outputTranscription,omitempty"`
	TurnComplete              bool                 `json:"turnComplete,omitempty"`
	GenerationComplete        bool                 `json:"generationComplete,omitempty"`
	Interrupted               bool                 `json:"interrupted,omitempty"`
}

type geminiTranscription struct {
	Text string `json:"text"`
}

type geminiToolCall struct {
	FunctionCalls []geminiFunctionCall `json:"functionCalls"`
}

type geminiFunctionCall struct {
	ID   string         `json:"id"`
	Name string         `json:"name"`
	Args map[string]any `json:"args"`
}

type geminiToolCallCancellation struct {
	IDs []string `json:"ids"`
}

type geminiWireError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Status  string `json:"status"`
}

func geminiSetupFor(cfgModel, voice, instructions string, hasHistory bool) geminiSetupMessage {
	setup := geminiSetup{
		Model: "models/" + strings.TrimPrefix(strings.TrimSpace(cfgModel), "models/"),
		GenerationConfig: geminiGenerationConfig{
			ResponseModalities: []string{"AUDIO"},
			SpeechConfig: geminiSpeechConfig{VoiceConfig: geminiVoiceConfig{
				PrebuiltVoiceConfig: geminiPrebuiltVoiceConfig{VoiceName: voice},
			}},
		},
		SystemInstruction: geminiContent{Parts: []geminiPart{{Text: instructions}}},
		Tools: []geminiTool{{FunctionDeclarations: []geminiFunctionDeclaration{{
			Name:        geminiDelegationTool,
			Description: "Run coding, file, shell, tool, or project-specific work in the term-llm execution backend.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"input": map[string]any{"type": "string", "description": "The complete task for the execution backend."},
				},
				"required": []string{"input"},
			},
		}}}},
		InputAudioTranscription:  map[string]any{},
		OutputAudioTranscription: map[string]any{},
		RealtimeInputConfig:      geminiRealtimeInputConfig{ActivityHandling: "START_OF_ACTIVITY_INTERRUPTS"},
	}
	if hasHistory {
		setup.HistoryConfig = &geminiHistoryConfig{InitialHistoryInClientContent: true}
	}
	return geminiSetupMessage{Setup: setup}
}

func geminiHistory(items []InitialItem) *geminiClientMessage {
	turns := make([]geminiContent, 0, len(items))
	for _, item := range items {
		text := strings.TrimSpace(item.Text)
		if text == "" {
			continue
		}
		role := "user"
		if item.Role == RoleAssistant {
			role = "model"
		} else if item.Role == "system" || item.Role == "developer" {
			text = "[CONTEXT] " + text
		}
		turns = append(turns, geminiContent{Role: role, Parts: []geminiPart{{Text: text}}})
	}
	if len(turns) == 0 {
		return nil
	}
	return &geminiClientMessage{ClientContent: &geminiClientContent{Turns: turns, TurnComplete: true}}
}

func geminiDelegationInput(call geminiFunctionCall) (string, error) {
	if call.Name != geminiDelegationTool {
		return "", fmt.Errorf("unsupported Gemini Live function %q", call.Name)
	}
	value, ok := call.Args["input"]
	if !ok {
		return "", fmt.Errorf("Gemini Live delegation %q is missing input", call.ID)
	}
	text, ok := value.(string)
	if !ok || strings.TrimSpace(text) == "" {
		return "", fmt.Errorf("Gemini Live delegation %q has invalid input", call.ID)
	}
	return strings.TrimSpace(text), nil
}

func geminiErrorText(wire *geminiWireError) string {
	if wire == nil {
		return "Gemini Live session error"
	}
	if text := strings.TrimSpace(wire.Message); text != "" {
		return text
	}
	if text := strings.TrimSpace(wire.Status); text != "" {
		return text
	}
	if wire.Code != 0 {
		return fmt.Sprintf("Gemini Live error %d", wire.Code)
	}
	return "Gemini Live session error"
}

func marshalGemini(message any) ([]byte, error) {
	payload, err := json.Marshal(message)
	if err != nil {
		return nil, fmt.Errorf("encode Gemini Live message: %w", err)
	}
	return payload, nil
}
