package reasoning

import (
	"strings"
	"testing"
)

func TestAppendStreamItemTextSeparatesProviderItems(t *testing.T) {
	var builder strings.Builder
	var itemID string

	AppendStreamItemText(&builder, &itemID, "**Drafting joke**", "rs_1")
	AppendStreamItemText(&builder, &itemID, "", "rs_1")
	AppendStreamItemText(&builder, &itemID, "**Evaluating joke**", "rs_2")

	if got, want := builder.String(), "**Drafting joke**\n\n**Evaluating joke**"; got != want {
		t.Fatalf("content = %q, want %q", got, want)
	}
}

func TestAppendStreamItemTextPreservesSameItemDeltas(t *testing.T) {
	var builder strings.Builder
	var itemID string

	AppendStreamItemText(&builder, &itemID, "**Draft", "rs_1")
	AppendStreamItemText(&builder, &itemID, "ing**", "rs_1")

	if got, want := builder.String(), "**Drafting**"; got != want {
		t.Fatalf("content = %q, want %q", got, want)
	}
}

func TestAppendStreamItemTextKeepsBoundaryAfterEmptyNewItemEvent(t *testing.T) {
	var builder strings.Builder
	var itemID string

	AppendStreamItemText(&builder, &itemID, "**First**", "rs_1")
	AppendStreamItemText(&builder, &itemID, "", "rs_2")
	AppendStreamItemText(&builder, &itemID, "**Second**", "rs_2")

	if got, want := builder.String(), "**First**\n\n**Second**"; got != want {
		t.Fatalf("content = %q, want %q", got, want)
	}
}

func TestAppendStreamItemTextTreatsCRLFAsOneLineBreak(t *testing.T) {
	var builder strings.Builder
	var itemID string

	AppendStreamItemText(&builder, &itemID, "**First**\r\n", "rs_1")
	AppendStreamItemText(&builder, &itemID, "**Second**", "rs_2")

	if got, want := builder.String(), "**First**\r\n\n**Second**"; got != want {
		t.Fatalf("content = %q, want %q", got, want)
	}
}
