package run

import (
	"context"
	"errors"
	"testing"
)

func TestJoinErrorsPreservesSubstantiveLeaves(t *testing.T) {
	provider := errors.New("provider failed")
	other := errors.New("tool failed")
	cases := []struct {
		name       string
		existing   error
		additional error
		want       []error
	}{
		{name: "joined provider and cancel", existing: errors.Join(provider, context.Canceled), want: []error{provider, context.Canceled}},
		{name: "cancel and joined provider", existing: context.Canceled, additional: errors.Join(provider, context.Canceled), want: []error{provider, context.Canceled}},
		{name: "joined provider and other substantive", existing: errors.Join(provider, context.Canceled), additional: other, want: []error{provider, context.Canceled, other}},
		{name: "pure cancellation ladders drop", existing: errors.Join(context.Canceled, context.DeadlineExceeded), additional: provider, want: []error{provider}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := JoinErrors(tc.existing, tc.additional)
			for _, want := range tc.want {
				if !errors.Is(err, want) {
					t.Fatalf("JoinErrors = %v; missing %v", err, want)
				}
			}
		})
	}
}
