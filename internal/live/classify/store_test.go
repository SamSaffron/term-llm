package liveclassify

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestDecisionStoreRoundTripHonorsSinceAndOptionalState(t *testing.T) {
	store, err := OpenDecisionStore(filepath.Join(t.TempDir(), "live.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Now().UTC().Truncate(time.Millisecond)
	for _, row := range []DecisionRecord{
		{CreatedAt: now.Add(-time.Hour), LiveID: "old", BoundSession: "a", Probabilities: map[string]float64{"steer": 1}, GatedLabel: "steer", ActedLabel: "steer", Latency: 3 * time.Millisecond},
		{CreatedAt: now, LiveID: "new", BoundSession: "b", StateJSON: []byte(`{"message":"private speech"}`), Probabilities: map[string]float64{"status": 0.9}, GatedLabel: "status", ActedLabel: "status", ResolverOutcome: "", Latency: 17 * time.Millisecond},
	} {
		if err := store.Insert(context.Background(), row); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := store.List(context.Background(), now.Add(-time.Minute), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].LiveID != "new" || string(rows[0].StateJSON) != `{"message":"private speech"}` || rows[0].Probabilities["status"] != 0.9 || rows[0].Latency != 17*time.Millisecond {
		t.Fatalf("rows = %+v", rows)
	}
	all, err := store.List(context.Background(), time.Time{}, 10)
	if err != nil || len(all) != 2 || len(all[1].StateJSON) != 0 {
		t.Fatalf("all rows = %+v, %v", all, err)
	}
}
