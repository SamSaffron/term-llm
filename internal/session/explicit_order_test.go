package session

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestPermuteOrder(t *testing.T) {
	current := []rankedRow{{id: "a", rank: 1}, {id: "b", rank: 2}, {id: "c", rank: 3}, {id: "d", rank: 4}}
	for _, tc := range []struct {
		ids  []string
		want []string
	}{
		{ids: []string{"d", "a"}, want: []string{"d", "b", "c", "a"}},
		{ids: []string{"b"}, want: []string{"a", "b", "c", "d"}},
		{ids: []string{"c", "b", "a"}, want: []string{"c", "b", "a", "d"}},
		{ids: []string{"b", "d", "a", "c"}, want: []string{"b", "d", "a", "c"}},
	} {
		if got := permuteOrder(current, tc.ids); !reflect.DeepEqual(got, tc.want) {
			t.Fatalf("permute %v = %v, want %v", tc.ids, got, tc.want)
		}
	}
}

func TestNormalizeOrderIDs(t *testing.T) {
	invalid := errors.New("invalid order")
	got, err := normalizeOrderIDs([]string{" b ", "a"}, 2, invalid, "project")
	if err != nil || !reflect.DeepEqual(got, []string{"b", "a"}) {
		t.Fatalf("normalize = %v, %v; want trimmed [b a]", got, err)
	}
	for _, tc := range []struct {
		name string
		ids  []string
		want string
	}{
		{name: "empty", ids: nil, want: "no projects listed"},
		{name: "oversized", ids: []string{"a", "b", "c"}, want: "at most 2 projects"},
		{name: "blank", ids: []string{"a", " "}, want: "project id is empty"},
		{name: "duplicate", ids: []string{"a", " a"}, want: "project a is listed more than once"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := normalizeOrderIDs(tc.ids, 2, invalid, "project")
			if !errors.Is(err, invalid) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %v containing %q", err, invalid, tc.want)
			}
		})
	}
}
