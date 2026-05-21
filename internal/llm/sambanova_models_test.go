package llm

import "testing"

func TestPricingForProviderModelSambaNova(t *testing.T) {
	input, output, ok := PricingForProviderModel("sambanova", "gpt-oss-120b")
	if !ok {
		t.Fatal("expected SambaNova pricing")
	}
	if input != 0.22 || output != 0.59 {
		t.Fatalf("pricing = %g/%g, want 0.22/0.59", input, output)
	}
}

func TestSambaNovaCuratedModelsUseCurrentLimits(t *testing.T) {
	if got := InputLimitForProviderModel("sambanova", "DeepSeek-V3.2"); got != 32_000 {
		t.Fatalf("DeepSeek-V3.2 limit = %d, want 32000", got)
	}
	if got := InputLimitForProviderModel("sambanova", "MiniMax-M2.7"); got != 192_000 {
		t.Fatalf("MiniMax-M2.7 limit = %d, want 192000", got)
	}
}
