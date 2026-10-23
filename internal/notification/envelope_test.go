package notification

import (
	"regexp"
	"testing"
)

func TestTopic(t *testing.T) {
	const eventID = "completion:turn-a:destination-a"
	const want = "kr8RxINZs4KucVFfvjcevDnOYwuwMS_p"
	// Pin the wire algorithm independently from producer/consumer agreement.
	if got := Topic(eventID); got != want {
		t.Fatalf("Topic(%q) = %q, want %q", eventID, got, want)
	}
	if Topic(eventID) != Topic(eventID) || Topic(eventID) == Topic("completion:turn-b:destination-a") {
		t.Fatal("topic must be stable per event and distinct across events")
	}
	if !regexp.MustCompile(`^[A-Za-z0-9_-]{32}$`).MatchString(Topic(eventID)) {
		t.Fatal("topic is not URL-safe or exceeds the Web Push topic bound")
	}
}
