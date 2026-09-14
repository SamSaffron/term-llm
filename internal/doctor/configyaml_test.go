package doctor

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func parseYAMLFixture(t *testing.T, source string) *yaml.Node {
	t.Helper()
	var root yaml.Node
	if err := yaml.Unmarshal([]byte(source), &root); err != nil {
		t.Fatalf("parse yaml: %v", err)
	}
	return &root
}

func TestCollectUnknownKeysStopsAtTheOutermostUnknownKey(t *testing.T) {
	root := parseYAMLFixture(t, `
default_provider: openai
mystery:
  nested:
    deeper: 1
sessions:
  enabled: true
  bogus_session_key: 1
`)

	unknown := collectUnknownKeys(root)

	var paths []string
	for _, key := range unknown {
		paths = append(paths, key.path)
	}
	want := []string{"mystery", "sessions.bogus_session_key"}
	if strings.Join(paths, ",") != strings.Join(want, ",") {
		t.Fatalf("got %v, want %v", paths, want)
	}
	if unknown[0].line != 3 {
		t.Fatalf("expected the mystery key on line 3, got %d", unknown[0].line)
	}
}

func TestCollectUnknownKeysAcceptsDynamicSections(t *testing.T) {
	root := parseYAMLFixture(t, `
providers:
  my-custom-endpoint:
    type: openai_compatible
    env:
      ANY_VARIABLE: value
agents:
  preferences:
    reviewer:
      provider: openai
`)

	if unknown := collectUnknownKeys(root); len(unknown) != 0 {
		t.Fatalf("dynamic provider and agent keys are valid, got %+v", unknown)
	}
}

func TestSuggestKey(t *testing.T) {
	cases := []struct {
		name string
		path string
		want string
	}{
		{name: "top level typo", path: "deafult_provider", want: "default_provider"},
		{name: "nested typo", path: "sessions.enabeld", want: "sessions.enabled"},
		{name: "provider field typo", path: "providers.openai.api_kye", want: "providers.openai.api_key"},
		{name: "nothing close", path: "wildly_unrelated_nonsense_key", want: ""},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := suggestKey(testCase.path); got != testCase.want {
				t.Fatalf("got %q, want %q", got, testCase.want)
			}
		})
	}
}

func TestCommentOutYAMLKeyCommentsWholeBlock(t *testing.T) {
	source := `# leading comment
default_provider: openai

mystery:
  nested: value      # trailing comment
  list:
    - one
    - two

sessions:
  enabled: true
`

	updated, err := commentOutYAMLKey([]byte(source), "mystery")
	if err != nil {
		t.Fatalf("comment out: %v", err)
	}

	text := string(updated)
	for _, want := range []string{
		"# leading comment",
		"default_provider: openai",
		"# unknown key commented out by term-llm doctor",
		"# mystery:",
		"#   nested: value      # trailing comment",
		"#     - two",
		"sessions:\n  enabled: true",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("expected %q in:\n%s", want, text)
		}
	}

	// The result must still parse, and the commented key must be gone.
	var parsed map[string]any
	if err := yaml.Unmarshal(updated, &parsed); err != nil {
		t.Fatalf("result does not parse: %v\n%s", err, text)
	}
	if _, present := parsed["mystery"]; present {
		t.Fatalf("expected the key to be removed from the parsed document:\n%s", text)
	}
	if parsed["default_provider"] != "openai" {
		t.Fatalf("expected unrelated values to survive:\n%s", text)
	}
}

func TestCommentOutYAMLKeyHandlesNestedAndTrailingKeys(t *testing.T) {
	source := `sessions:
  enabled: true
  bogus:
    a: 1
`

	updated, err := commentOutYAMLKey([]byte(source), "sessions.bogus")
	if err != nil {
		t.Fatalf("comment out: %v", err)
	}

	var parsed map[string]map[string]any
	if err := yaml.Unmarshal(updated, &parsed); err != nil {
		t.Fatalf("result does not parse: %v\n%s", err, updated)
	}
	if _, present := parsed["sessions"]["bogus"]; present {
		t.Fatalf("expected the nested key to be removed:\n%s", updated)
	}
	if parsed["sessions"]["enabled"] != true {
		t.Fatalf("expected the sibling key to survive:\n%s", updated)
	}
	if !strings.Contains(string(updated), "  # bogus:") {
		t.Fatalf("expected the original indentation to be preserved:\n%s", updated)
	}
}

func TestCommentOutYAMLKeyIgnoresMissingKey(t *testing.T) {
	updated, err := commentOutYAMLKey([]byte("default_provider: openai\n"), "absent")
	if err != nil {
		t.Fatalf("comment out: %v", err)
	}
	if updated != nil {
		t.Fatalf("expected no rewrite for a missing key, got %q", updated)
	}
}

func TestLevenshtein(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{a: "", b: "", want: 0},
		{a: "abc", b: "abc", want: 0},
		{a: "abc", b: "abd", want: 1},
		{a: "kitten", b: "sitting", want: 3},
		{a: "", b: "abc", want: 3},
	}
	for _, testCase := range cases {
		if got := levenshtein(testCase.a, testCase.b); got != testCase.want {
			t.Fatalf("levenshtein(%q, %q) = %d, want %d", testCase.a, testCase.b, got, testCase.want)
		}
	}
}
