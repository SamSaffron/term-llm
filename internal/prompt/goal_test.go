package prompt

import (
	"strings"
	"testing"
)

func TestBuildGoalPromptEscapesObjectiveAndBudget(t *testing.T) {
	goal := GoalPromptData{Objective: `finish <danger> & "ship"`, TokenBudget: 100, TokensUsed: 40}
	prompt := BuildGoalPrompt(goal, GoalPromptContinuation)
	if !strings.Contains(prompt, "finish &lt;danger&gt; &amp; &#34;ship&#34;") {
		t.Fatalf("objective was not escaped in prompt:\n%s", prompt)
	}
	if !strings.Contains(prompt, "Tokens remaining: 60") {
		t.Fatalf("remaining budget missing from prompt:\n%s", prompt)
	}
}
