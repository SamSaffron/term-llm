package agyproxy

import (
	"encoding/json"
	"strings"
	"testing"
)

const agyRestartSystemMessage = "The following is a <SYSTEM_MESSAGE> not actually sent by the user. It is provided by the system as important information to pay attention to.\n\n" +
	"<SYSTEM_MESSAGE>\n[Message] timestamp=2026-10-16T03:06:56Z sender=system priority=MESSAGE_PRIORITY_LOW content=[Notice] All your subagents and background tasks have been stopped due to server restart. If you want a subagent to continue working, it needs to be revived by sending it a new message. If resuming work, please check on status and restart as needed.\n</SYSTEM_MESSAGE>"

func TestFilterGenerationRequestStripsAgyHarnessChatter(t *testing.T) {
	body := []byte(`{"request":{
		"tools":[{"functionDeclarations":[{"name":"mcp_term-llm_shell","description":"This is a tool from the term-llm MCP server.\nExecute a shell command.",
			"parameters":{"type":"OBJECT","properties":{"command":{"type":"STRING"},"toolSummary":{"type":"STRING"}},"required":["command","toolSummary"]}}]}],
		"contents":[
			{"role":"user","parts":[{"text":"<USER_REQUEST>\nhi\n</USER_REQUEST>\n<USER_SETTINGS_CHANGE>\nThe user changed setting ` + "`Model Selection`" + ` from None to Gemini 4 Argon (Low). No need to comment.\n</USER_SETTINGS_CHANGE>"}]},
			{"role":"model","parts":[{"functionCall":{"name":"mcp_term-llm_shell","args":{"command":"echo hi","toolSummary":"Echo"}}}]},
			{"role":"model","parts":[{"functionResponse":{"name":"mcp_term-llm_shell","response":{"output":"Created At: 2026-10-16T13:56:12+11:00\nCompleted At: 2026-10-16T13:56:12+11:00\nhi\nCreated At: kept"}}}]},
			{"role":"user","parts":[{"text":"<USER_REQUEST>\nagain\n</USER_REQUEST>"}]},
			{"role":"user","parts":[{"text":` + mustJSON(agyRestartSystemMessage) + `}]}
		]}}`)
	out, err := FilterGenerationRequest(body, true)
	if err != nil {
		t.Fatal(err)
	}
	var root struct {
		Request struct {
			Tools []struct {
				FunctionDeclarations []struct {
					Description string `json:"description"`
					Parameters  struct {
						Required []string `json:"required"`
					} `json:"parameters"`
				} `json:"functionDeclarations"`
			} `json:"tools"`
			Contents []struct {
				Role  string `json:"role"`
				Parts []struct {
					Text             string `json:"text"`
					FunctionResponse *struct {
						Response struct {
							Output string `json:"output"`
						} `json:"response"`
					} `json:"functionResponse"`
				} `json:"parts"`
			} `json:"contents"`
		} `json:"request"`
	}
	if err := json.Unmarshal(out, &root); err != nil {
		t.Fatal(err)
	}
	decl := root.Request.Tools[0].FunctionDeclarations[0]
	if decl.Description != "Execute a shell command." {
		t.Fatalf("description = %q", decl.Description)
	}
	// agy validates toolSummary before dispatch, so it must stay required.
	if strings.Join(decl.Parameters.Required, ",") != "command,toolSummary" {
		t.Fatalf("required = %v", decl.Parameters.Required)
	}
	contents := root.Request.Contents
	if len(contents) != 4 {
		t.Fatalf("contents = %+v, want restart notice removed", contents)
	}
	if got := contents[0].Parts[0].Text; got != "<USER_REQUEST>\nhi\n</USER_REQUEST>" {
		t.Fatalf("settings change survived: %q", got)
	}
	if got := contents[2].Parts[0].FunctionResponse.Response.Output; got != "hi\nCreated At: kept" {
		t.Fatalf("tool output = %q", got)
	}
}

func TestIsAgyRestartNoticeKeepsOtherSystemMessages(t *testing.T) {
	if !isAgyRestartNotice(agyRestartSystemMessage) {
		t.Fatal("restart notice not recognized")
	}
	combined := strings.Replace(agyRestartSystemMessage, "</SYSTEM_MESSAGE>",
		"[Message] timestamp=now sender=subagent content=real work finished\n</SYSTEM_MESSAGE>", 1)
	if isAgyRestartNotice(combined) {
		t.Fatal("system message with another message was treated as restart-only")
	}
	if isAgyRestartNotice("User quoted: " + agyRestartSystemMessage) {
		t.Fatal("quoted notice inside user text was treated as agy's notice")
	}
}

func TestCleanAgyContentsPreservesConversationText(t *testing.T) {
	block := "<USER_SETTINGS_CHANGE>\nexample\n</USER_SETTINGS_CHANGE>"
	userText := "<USER_REQUEST>\nExplain this block:\n" + block + "\n</USER_REQUEST>"
	modelText := "The block was:\n" + block
	request := map[string]any{"contents": []any{
		map[string]any{"role": "user", "parts": []any{map[string]any{"text": userText + "\n" + block}}},
		map[string]any{"role": "model", "parts": []any{map[string]any{"text": modelText}}},
		map[string]any{"role": "model", "parts": []any{map[string]any{"text": agyRestartSystemMessage}}},
	}}
	cleanAgyContents(request)
	contents := request["contents"].([]any)
	text := func(i int) string {
		return contents[i].(map[string]any)["parts"].([]any)[0].(map[string]any)["text"].(string)
	}
	if len(contents) != 3 {
		t.Fatalf("contents = %d, want model-role notice-shaped text kept", len(contents))
	}
	if got := text(0); got != userText {
		t.Fatalf("user text = %q, want request body kept and trailing notice removed", got)
	}
	if got := text(1); got != modelText {
		t.Fatalf("model text = %q", got)
	}
}

func mustJSON(value string) string {
	data, _ := json.Marshal(value)
	return string(data)
}
