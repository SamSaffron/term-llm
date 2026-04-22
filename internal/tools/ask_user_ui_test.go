package tools

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

func testAskQuestion(header string, multiSelect bool) []AskUserQuestion {
	return []AskUserQuestion{
		{
			Header:      header,
			Question:    "Choose an option",
			MultiSelect: multiSelect,
			Options: []AskUserOption{
				{Label: "Option A", Description: "First option"},
				{Label: "Option B", Description: "Second option"},
			},
		},
	}
}

func TestAskUserUI_SingleMultiSelect_EnterSubmitsWhenAnswered_Standalone(t *testing.T) {
	m := newAskModel(testAskQuestion("Q1", true))

	// Select first option.
	model, _ := m.Update(tea.KeyPressMsg{Code: tea.KeySpace})
	m = model.(*AskUserModel)

	if len(m.answers[0].selected) != 1 {
		t.Fatalf("expected one selected option after space, got %d", len(m.answers[0].selected))
	}

	// Submit.
	model, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = model.(*AskUserModel)

	if !m.Done {
		t.Fatal("expected Done=true after pressing enter with a selected option")
	}
	if m.Cancelled {
		t.Fatal("expected Cancelled=false")
	}

	answers := m.Answers()
	if len(answers) != 1 {
		t.Fatalf("expected 1 answer, got %d", len(answers))
	}
	if !answers[0].IsMultiSelect {
		t.Fatal("expected IsMultiSelect=true")
	}
	if len(answers[0].SelectedList) != 1 || answers[0].SelectedList[0] != "Option A" {
		t.Fatalf("expected SelectedList=[Option A], got %#v", answers[0].SelectedList)
	}
}

func TestAskUserUI_SingleMultiSelect_EnterDoesNotSubmitWhenEmpty_Standalone(t *testing.T) {
	m := newAskModel(testAskQuestion("Q1", true))

	model, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = model.(*AskUserModel)

	if m.Done {
		t.Fatal("expected Done=false when pressing enter with no selected options")
	}
}

func TestAskUserUI_SingleMultiSelect_SpaceToggles_Standalone(t *testing.T) {
	m := newAskModel(testAskQuestion("Q1", true))

	model, _ := m.Update(tea.KeyPressMsg{Code: tea.KeySpace})
	m = model.(*AskUserModel)
	if len(m.answers[0].selected) != 1 || m.answers[0].selected[0] != 0 {
		t.Fatalf("expected selection [0] after first space, got %#v", m.answers[0].selected)
	}

	model, _ = m.Update(tea.KeyPressMsg{Code: tea.KeySpace})
	m = model.(*AskUserModel)
	if len(m.answers[0].selected) != 0 {
		t.Fatalf("expected empty selection after second space, got %#v", m.answers[0].selected)
	}
}

func TestAskUserUI_SingleMultiSelect_EnterSubmitsWhenAnswered_Embedded(t *testing.T) {
	m := NewEmbeddedAskUserModel(testAskQuestion("Q1", true), 80)

	m.UpdateEmbedded(tea.KeyPressMsg{Code: tea.KeySpace})
	if len(m.answers[0].selected) != 1 {
		t.Fatalf("expected one selected option after space, got %d", len(m.answers[0].selected))
	}

	m.UpdateEmbedded(tea.KeyPressMsg{Code: tea.KeyEnter})
	if !m.Done {
		t.Fatal("expected Done=true after pressing enter with a selected option")
	}
}

func TestAskUserUI_MultiQuestionMultiSelect_EnterStillToggles(t *testing.T) {
	questions := []AskUserQuestion{
		{
			Header:      "Q1",
			Question:    "Pick one or more",
			MultiSelect: true,
			Options: []AskUserOption{
				{Label: "Option A", Description: "First option"},
				{Label: "Option B", Description: "Second option"},
			},
		},
		{
			Header:      "Q2",
			Question:    "Pick one",
			MultiSelect: false,
			Options: []AskUserOption{
				{Label: "Choice 1", Description: "First choice"},
				{Label: "Choice 2", Description: "Second choice"},
			},
		},
	}
	m := newAskModel(questions)

	// In multi-question mode, enter should continue toggling selection for multi-select.
	model, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = model.(*AskUserModel)

	if m.Done {
		t.Fatal("expected Done=false for multi-question flow")
	}
	if m.currentTab != 0 {
		t.Fatalf("expected to remain on first question tab, got %d", m.currentTab)
	}
	if len(m.answers[0].selected) != 1 || m.answers[0].selected[0] != 0 {
		t.Fatalf("expected selection [0] after enter toggle, got %#v", m.answers[0].selected)
	}
}

func TestAskUserUI_HelpText_SingleMultiSelect_ShowsSubmitHint(t *testing.T) {
	m := newAskModel(testAskQuestion("Q1", true))

	help := m.renderHelp()
	if !strings.Contains(help, "space toggle") {
		t.Fatalf("expected help to contain %q, got %q", "space toggle", help)
	}
	if !strings.Contains(help, "enter submit") {
		t.Fatalf("expected help to contain %q, got %q", "enter submit", help)
	}
}

func TestAskUserUI_CustomInputAcceptsPaste_Standalone(t *testing.T) {
	m := newAskModel(testAskQuestion("Q1", false))
	m.cursor = len(m.questions[0].Options)
	m.textInput.Focus()

	model, _ := m.Update(tea.PasteMsg{Content: "custom answer"})
	m = model.(*AskUserModel)

	view := m.View().Content
	if !strings.Contains(view, "custom answer") {
		t.Fatalf("expected pasted custom answer in view, got %q", view)
	}
}

func TestAskUserUI_CustomInputAcceptsPaste_Embedded(t *testing.T) {
	m := NewEmbeddedAskUserModel(testAskQuestion("Q1", false), 80)
	m.cursor = len(m.questions[0].Options)
	m.textInput.Focus()

	m.UpdateEmbedded(tea.PasteMsg{Content: "custom answer"})

	view := m.View().Content
	if !strings.Contains(view, "custom answer") {
		t.Fatalf("expected pasted custom answer in view, got %q", view)
	}
}

func TestAskUserUI_NextUnansweredTab(t *testing.T) {
	questions := []AskUserQuestion{
		{
			Header:      "Q1",
			Question:    "Question 1",
			MultiSelect: false,
			Options:     []AskUserOption{{Label: "A"}},
		},
		{
			Header:      "Q2",
			Question:    "Question 2",
			MultiSelect: false,
			Options:     []AskUserOption{{Label: "B"}},
		},
		{
			Header:      "Q3",
			Question:    "Question 3",
			MultiSelect: false,
			Options:     []AskUserOption{{Label: "C"}},
		},
	}
	m := newAskModel(questions)
	m.currentTab = 0
	m.answers[0].text = "A"
	m.answers[2].text = "C"

	if got, want := m.nextUnansweredTab(), 1; got != want {
		t.Fatalf("nextUnansweredTab() = %d, want %d", got, want)
	}

	m.answers[1].text = "B"
	if got, want := m.nextUnansweredTab(), len(questions); got != want {
		t.Fatalf("nextUnansweredTab() when all answered = %d, want %d", got, want)
	}
}

func TestAskUserUI_StandaloneAndEmbeddedStayInSyncForCustomAnswerFlow(t *testing.T) {
	questions := []AskUserQuestion{
		{
			Header:      "Q1",
			Question:    "Question 1",
			MultiSelect: false,
			Options: []AskUserOption{
				{Label: "A"},
				{Label: "B"},
			},
		},
		{
			Header:      "Q2",
			Question:    "Question 2",
			MultiSelect: false,
			Options: []AskUserOption{
				{Label: "C"},
				{Label: "D"},
			},
		},
	}

	standalone := newAskModel(questions)
	embedded := NewEmbeddedAskUserModel(questions, 80)

	messages := []tea.Msg{
		tea.KeyPressMsg{Code: tea.KeyDown},
		tea.KeyPressMsg{Code: tea.KeyDown},
		tea.PasteMsg{Content: "custom answer"},
		tea.KeyPressMsg{Code: tea.KeyEnter},
	}

	for _, msg := range messages {
		model, _ := standalone.Update(msg)
		standalone = model.(*AskUserModel)
		embedded.UpdateEmbedded(msg)
	}

	if standalone.currentTab != embedded.currentTab {
		t.Fatalf("currentTab standalone=%d embedded=%d", standalone.currentTab, embedded.currentTab)
	}
	if standalone.cursor != embedded.cursor {
		t.Fatalf("cursor standalone=%d embedded=%d", standalone.cursor, embedded.cursor)
	}
	if standalone.answers[0].text != embedded.answers[0].text {
		t.Fatalf("answer text standalone=%q embedded=%q", standalone.answers[0].text, embedded.answers[0].text)
	}
	if standalone.answers[0].isCustom != embedded.answers[0].isCustom {
		t.Fatalf("isCustom standalone=%v embedded=%v", standalone.answers[0].isCustom, embedded.answers[0].isCustom)
	}
}
