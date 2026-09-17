package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestNormalizeAskUserAnswers(t *testing.T) {
	t.Parallel()

	questions := []AskUserQuestion{
		{
			Header:   "Color",
			Question: "Pick a color",
			Options: []AskUserOption{
				{Label: "Red", Description: "Warm"},
				{Label: "Blue", Description: "Cool"},
			},
		},
		{
			Header:      "Tools",
			Question:    "Pick tools",
			MultiSelect: true,
			Options: []AskUserOption{
				{Label: "Go", Description: "Compiler"},
				{Label: "Docker", Description: "Container"},
				{Label: "Git", Description: "Version control"},
			},
		},
	}

	t.Run("canonicalizes valid answers", func(t *testing.T) {
		answers, err := NormalizeAskUserAnswers(questions, []AskUserAnswer{
			{Selected: " Blue ", IsCustom: false, Header: "ignored", QuestionIndex: 99},
			{SelectedList: []string{"Go", "Git"}, IsMultiSelect: true},
		})
		if err != nil {
			t.Fatalf("NormalizeAskUserAnswers error = %v", err)
		}
		if answers[0].QuestionIndex != 0 || answers[0].Header != "Color" || answers[0].Selected != "Blue" {
			t.Fatalf("single-select answer not canonicalized: %#v", answers[0])
		}
		if answers[1].QuestionIndex != 1 || answers[1].Header != "Tools" {
			t.Fatalf("multi-select metadata not canonicalized: %#v", answers[1])
		}
		if got := strings.Join(answers[1].SelectedList, ","); got != "Go,Git" {
			t.Fatalf("multi-select list = %q, want %q", got, "Go,Git")
		}
		if answers[1].Selected != "Go, Git" {
			t.Fatalf("multi-select Selected = %q, want %q", answers[1].Selected, "Go, Git")
		}
	})

	t.Run("allows custom single-select answers", func(t *testing.T) {
		answers, err := NormalizeAskUserAnswers(questions[:1], []AskUserAnswer{{Selected: "Chartreuse", IsCustom: true}})
		if err != nil {
			t.Fatalf("NormalizeAskUserAnswers error = %v", err)
		}
		if !answers[0].IsCustom || answers[0].Selected != "Chartreuse" {
			t.Fatalf("custom answer = %#v", answers[0])
		}
	})

	t.Run("rejects invalid predefined single-select values", func(t *testing.T) {
		_, err := NormalizeAskUserAnswers(questions[:1], []AskUserAnswer{{Selected: "Green"}})
		if err == nil || !strings.Contains(err.Error(), "invalid selection") {
			t.Fatalf("error = %v, want invalid selection", err)
		}
	})

	t.Run("rejects empty multi-select answers", func(t *testing.T) {
		_, err := NormalizeAskUserAnswers(questions[1:], []AskUserAnswer{{}})
		if err == nil || !strings.Contains(err.Error(), "at least one selection") {
			t.Fatalf("error = %v, want at least one selection", err)
		}
	})

	t.Run("rejects duplicate multi-select values", func(t *testing.T) {
		_, err := NormalizeAskUserAnswers(questions[1:], []AskUserAnswer{{SelectedList: []string{"Go", "Go"}}})
		if err == nil || !strings.Contains(err.Error(), "duplicate selection") {
			t.Fatalf("error = %v, want duplicate selection", err)
		}
	})
}

func TestAskUserSuccessfulAnswerIsTrustedGuardianInput(t *testing.T) {
	t.Parallel()
	ctx := ContextWithAskUserUIFunc(context.Background(), func(context.Context, []AskUserQuestion) ([]AskUserAnswer, error) {
		return []AskUserAnswer{{Selected: "Yes, delete exactly /tmp/abc"}}, nil
	})
	args, err := json.Marshal(AskUserArgs{Questions: []AskUserQuestion{{
		Header: "Delete path", Question: "Delete exactly /tmp/abc?",
		Options: []AskUserOption{{Label: "Cancel", Description: "Do not delete"}, {Label: "Yes, delete exactly /tmp/abc", Description: "Authorize this exact deletion"}},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	output, err := NewAskUserTool().Execute(ctx, args)
	if err != nil || !strings.Contains(output.TrustedUserInput, "Yes, delete exactly /tmp/abc. Authorize this exact deletion") {
		t.Fatalf("ask_user output = %+v, err = %v", output, err)
	}
	if !strings.Contains(output.Content, "Yes, delete exactly /tmp/abc") {
		t.Fatalf("ask_user output lost answer: %s", output.Content)
	}
	var result AskUserResult
	if err := json.Unmarshal([]byte(output.Content), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Questions) != 1 || result.Questions[0].Question != "Delete exactly /tmp/abc?" {
		t.Fatalf("trusted ask_user output lost the question that was answered: %+v", result)
	}
}

func TestAskUserAnswerSummary(t *testing.T) {
	t.Parallel()

	summary := AskUserAnswerSummary([]AskUserAnswer{
		{Header: "Color", Selected: "Blue"},
		{Header: "Tools", Selected: "Go, Git"},
	})
	if summary != "Color: Blue | Tools: Go, Git" {
		t.Fatalf("summary = %q", summary)
	}
}
