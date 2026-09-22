package tools

import "testing"

func TestFormatGuardianApprovalHumanReadable(t *testing.T) {
	t.Parallel()

	got := formatGuardianApproval(PolicyDecision{RiskLevel: "medium", UserAuthorization: "high"})
	want := "approved (medium risk; clearly user-authorized)"
	if got != want {
		t.Fatalf("formatGuardianApproval() = %q, want %q", got, want)
	}
}

func TestFormatGuardianApprovalMarksFallbackEscalation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		decision PolicyDecision
		want     string
	}{
		{
			name:     "escalated",
			decision: PolicyDecision{RiskLevel: "low", UserAuthorization: "high", Escalated: true},
			want:     "approved (low risk; clearly user-authorized; via fallback)",
		},
		{
			name:     "escalated without risk level",
			decision: PolicyDecision{Escalated: true},
			want:     "approved (reviewed risk; authorization unclear; via fallback)",
		},
		{
			name:     "not escalated",
			decision: PolicyDecision{RiskLevel: "low", UserAuthorization: "high"},
			want:     "approved (low risk; clearly user-authorized)",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := formatGuardianApproval(tc.decision); got != tc.want {
				t.Fatalf("formatGuardianApproval() = %q, want %q", got, tc.want)
			}
		})
	}
}
