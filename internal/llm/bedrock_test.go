package llm

import "testing"

func TestResolveBedrockModelID(t *testing.T) {
	tests := []struct {
		name    string
		model   string
		userMap map[string]string
		region  string
		want    string
	}{
		// Built-in translations with US region
		{"opus 4.7 us", "claude-opus-4-7", nil, "us-west-2", "us.anthropic.claude-opus-4-7-v1"},
		{"opus 4.6 us", "claude-opus-4-6", nil, "us-west-2", "us.anthropic.claude-opus-4-6-v1"},
		{"sonnet 4.6 us", "claude-sonnet-4-6", nil, "us-east-1", "us.anthropic.claude-sonnet-4-6"},
		{"haiku 4.5 us", "claude-haiku-4-5", nil, "us-west-2", "us.anthropic.claude-haiku-4-5-20251001-v1:0"},
		{"sonnet 4.5 us", "claude-sonnet-4-5", nil, "us-west-2", "us.anthropic.claude-sonnet-4-5-20250929-v1:0"},
		{"opus 4.5 us", "claude-opus-4-5", nil, "us-east-1", "us.anthropic.claude-opus-4-5-20251101-v1:0"},
		{"sonnet 4 us", "claude-sonnet-4", nil, "us-west-2", "us.anthropic.claude-sonnet-4-20250514-v1:0"},

		// EU region derives eu. prefix
		{"sonnet 4.6 eu", "claude-sonnet-4-6", nil, "eu-west-1", "eu.anthropic.claude-sonnet-4-6"},
		{"opus 4.6 eu", "claude-opus-4-6", nil, "eu-central-1", "eu.anthropic.claude-opus-4-6-v1"},

		// AP region derives ap. prefix
		{"sonnet 4.6 ap", "claude-sonnet-4-6", nil, "ap-southeast-1", "ap.anthropic.claude-sonnet-4-6"},
		{"haiku 4.5 ap", "claude-haiku-4-5", nil, "ap-northeast-1", "ap.anthropic.claude-haiku-4-5-20251001-v1:0"},

		// User model_map takes precedence over built-in + geo
		{"user map override", "claude-sonnet-4-6", map[string]string{
			"claude-sonnet-4-6": "arn:aws:bedrock:us-west-2:123456:application-inference-profile/abc123",
		}, "us-west-2", "arn:aws:bedrock:us-west-2:123456:application-inference-profile/abc123"},

		// User map with custom alias
		{"user custom alias", "my-model", map[string]string{
			"my-model": "us.anthropic.claude-opus-4-6-v1",
		}, "us-west-2", "us.anthropic.claude-opus-4-6-v1"},

		// Unknown model passes through
		{"unknown passthrough", "some-unknown-model", nil, "us-west-2", "some-unknown-model"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := resolveBedrockModelID(tt.model, tt.userMap, tt.region)
			if got != tt.want {
				t.Errorf("resolveBedrockModelID(%q, region=%q) = %q, want %q", tt.model, tt.region, got, tt.want)
			}
		})
	}
}

func TestBedrockGeoPrefix(t *testing.T) {
	tests := []struct {
		region string
		want   string
	}{
		{"us-east-1", "us"},
		{"us-west-2", "us"},
		{"eu-west-1", "eu"},
		{"eu-central-1", "eu"},
		{"ap-southeast-1", "ap"},
		{"ap-northeast-1", "ap"},
		{"", "us"},           // empty falls through to default
		{"me-south-1", "us"}, // unknown region falls through to us
		{"sa-east-1", "us"},  // South America falls through to us
	}
	for _, tt := range tests {
		t.Run(tt.region, func(t *testing.T) {
			got := bedrockGeoPrefix(tt.region)
			if got != tt.want {
				t.Errorf("bedrockGeoPrefix(%q) = %q, want %q", tt.region, got, tt.want)
			}
		})
	}
}

func TestIsQualifiedBedrockID(t *testing.T) {
	tests := []struct {
		model string
		want  bool
	}{
		{"anthropic.claude-sonnet-4-6", true},
		{"us.anthropic.claude-opus-4-6-v1", true},
		{"eu.anthropic.claude-sonnet-4-6", true},
		{"ap.anthropic.claude-haiku-4-5-20251001-v1:0", true},
		{"global.anthropic.claude-sonnet-4-6", true},
		{"arn:aws:bedrock:us-west-2:123456:application-inference-profile/abc", true},
		{"claude-sonnet-4-6", false},
		{"claude-opus-4-6", false},
		{"my-custom-model", false},
	}
	for _, tt := range tests {
		t.Run(tt.model, func(t *testing.T) {
			got := isQualifiedBedrockID(tt.model)
			if got != tt.want {
				t.Errorf("isQualifiedBedrockID(%q) = %v, want %v", tt.model, got, tt.want)
			}
		})
	}
}

func TestBedrockModelSuffixParsing(t *testing.T) {
	// Verify that -thinking and -1m suffixes are stripped before model resolution.
	// This tests the same parse chain used in NewBedrockProvider.
	tests := []struct {
		input        string
		wantBase     string
		wantAdaptive bool
		want1m       bool
	}{
		{"claude-sonnet-4-6", "claude-sonnet-4-6", false, false},
		{"claude-sonnet-4-6-thinking", "claude-sonnet-4-6", true, false},
		{"claude-sonnet-4-6-1m", "claude-sonnet-4-6", false, true},
		{"claude-sonnet-4-6-1m-thinking", "claude-sonnet-4-6", true, true},
		{"claude-opus-4-6-thinking", "claude-opus-4-6", true, false},
		{"claude-haiku-4-5-thinking", "claude-haiku-4-5", false, false}, // haiku uses budget, not adaptive
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			afterThinking, _, adaptive := parseModelThinking(tt.input)
			base, use1m := parseModel1m(afterThinking)
			if base != tt.wantBase {
				t.Errorf("base = %q, want %q", base, tt.wantBase)
			}
			if adaptive != tt.wantAdaptive {
				t.Errorf("adaptive = %v, want %v", adaptive, tt.wantAdaptive)
			}
			if use1m != tt.want1m {
				t.Errorf("use1m = %v, want %v", use1m, tt.want1m)
			}

			// After suffix stripping, model should resolve to a Bedrock ID
			bedrockID := resolveBedrockModelID(base, nil, "us-west-2")
			if _, ok := bedrockBaseModelMap[base]; ok && bedrockID == base {
				t.Errorf("expected %q to be translated, got passthrough", base)
			}
		})
	}
}
