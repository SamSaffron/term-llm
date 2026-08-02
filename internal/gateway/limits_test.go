package gateway

import "testing"

func TestPerClientInferenceAndToolLimitsAreIsolatedAndReleased(t *testing.T) {
	server := &Server{limits: make(map[string]*clientLimits)}
	policy := Policy{
		MaxConcurrentInference: 1,
		SearchRatePerMinute:    60, SearchBurst: 5, MaxConcurrentSearch: 1,
		FetchRatePerMinute: 60, FetchBurst: 5, MaxConcurrentFetch: 1,
	}
	clientA := Client{ID: "a", Policy: policy}
	clientB := Client{ID: "b", Policy: policy}

	releaseA, ok := server.acquireInference(clientA)
	if !ok {
		t.Fatal("first client A inference permit denied")
	}
	if _, ok := server.acquireInference(clientA); ok {
		t.Fatal("client A exceeded inference concurrency cap")
	}
	releaseB, ok := server.acquireInference(clientB)
	if !ok {
		t.Fatal("client A usage leaked into client B inference cap")
	}
	releaseA()
	if releaseAgain, ok := server.acquireInference(clientA); !ok {
		t.Fatal("client A inference permit was not released")
	} else {
		releaseAgain()
	}
	releaseB()

	releaseSearchA, _, ok := server.acquireTool(clientA, true)
	if !ok {
		t.Fatal("first client A search permit denied")
	}
	if _, code, ok := server.acquireTool(clientA, true); ok || code != "search_concurrency_limited" {
		t.Fatalf("second client A search = ok=%t code=%q", ok, code)
	}
	releaseSearchB, _, ok := server.acquireTool(clientB, true)
	if !ok {
		t.Fatal("client A search usage leaked into client B")
	}
	releaseSearchA()
	releaseSearchB()

	releaseFetch, _, ok := server.acquireTool(clientA, false)
	if !ok {
		t.Fatal("first fetch permit denied")
	}
	if _, code, ok := server.acquireTool(clientA, false); ok || code != "fetch_concurrency_limited" {
		t.Fatalf("second fetch = ok=%t code=%q", ok, code)
	}
	releaseFetch()
}
