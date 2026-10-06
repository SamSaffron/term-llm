package share

import (
	"strings"
	"testing"
)

func assetCapabilities(types ...string) Capabilities {
	return Capabilities{
		Protocol: Protocol, Version: Version,
		Provider:   Provider{ID: "acme", Name: "Acme"},
		Operations: []Operation{OperationCreate}, Visibilities: []Visibility{VisibilityUnlisted},
		DefaultVisibility: VisibilityUnlisted, AssetMediaTypes: types,
	}
}

func TestValidateCapabilitiesAssetMediaTypes(t *testing.T) {
	if err := ValidateCapabilities(assetCapabilities("image/png", "image/jpeg")); err != nil {
		t.Fatalf("valid asset types rejected: %v", err)
	}
	if !assetCapabilities("image/png").SupportsAssetMediaType(" Image/PNG ") {
		t.Fatal("asset media type lookup is not normalized")
	}
	for name, types := range map[string][]string{
		"duplicate":  {"image/png", "image/png"},
		"parameters": {"image/png; q=1"},
		"uppercase":  {"Image/PNG"},
		"empty":      {""},
		"too many":   strings.Split(strings.Repeat("image/x,", 17), ",")[:17],
	} {
		if err := ValidateCapabilities(assetCapabilities(types...)); err == nil {
			t.Errorf("%s asset types accepted", name)
		}
	}
}

func TestValidateRequestAcceptsAssetFiles(t *testing.T) {
	req := Request{RequestID: "r", Visibility: VisibilityUnlisted, Entrypoint: "index.html", Files: []File{
		{Name: "index.html", MediaType: "text/html; charset=utf-8", Role: "entrypoint", Content: []byte("<img src=\"assets/a.png\">")},
		{Name: "session.md", MediaType: "text/markdown; charset=utf-8", Role: "transcript", Content: []byte("![x](assets/a.png)")},
		{Name: "assets/a.png", MediaType: "image/png", Role: RoleAsset, Content: []byte{0x89, 'P', 'N', 'G'}},
	}}
	if err := ValidateRequest(req); err != nil {
		t.Fatal(err)
	}
}
