package cmd

import "testing"

func TestResolvePlanSearch(t *testing.T) {
	tests := []struct {
		name      string
		search    bool
		noSearch  bool
		wantValue bool
	}{
		{
			name:      "default enabled",
			search:    true,
			noSearch:  false,
			wantValue: true,
		},
		{
			name:      "disabled by no-search",
			search:    true,
			noSearch:  true,
			wantValue: false,
		},
		{
			name:      "explicit search false",
			search:    false,
			noSearch:  false,
			wantValue: false,
		},
		{
			name:      "both false and no-search",
			search:    false,
			noSearch:  true,
			wantValue: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := resolvePlanSearch(tt.search, tt.noSearch)
			if got != tt.wantValue {
				t.Fatalf("resolvePlanSearch(%v, %v) = %v, want %v", tt.search, tt.noSearch, got, tt.wantValue)
			}
		})
	}
}
