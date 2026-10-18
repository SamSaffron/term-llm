package main

import (
	"bytes"
	"os"
	"testing"

	"github.com/samsaffron/term-llm/internal/tsgen"
)

func TestAPITypesUpToDate(t *testing.T) {
	got, err := tsgen.Generate("../..")
	if err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile("../../frontend/src/api/generated/types.ts")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("generated API contract is stale; run make api-types")
	}
}
