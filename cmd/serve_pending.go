package cmd

import "slices"

func sortedPendingSnapshots[P, S any](pending map[string]*P, snapshot func(*P) S, createdAt func(S) int64) []S {
	if len(pending) == 0 {
		return nil
	}
	items := make([]S, 0, len(pending))
	for _, item := range pending {
		if item != nil {
			items = append(items, snapshot(item))
		}
	}
	slices.SortStableFunc(items, func(a, b S) int {
		switch aTime, bTime := createdAt(a), createdAt(b); {
		case aTime < bTime:
			return -1
		case aTime > bTime:
			return 1
		default:
			return 0
		}
	})
	return items
}
