package worker

import (
	"regexp"
	"sync"
	"testing"
	"time"
)

func TestEventHelperConcurrentSequenceAndFencing(t *testing.T) {
	claim := testClaim(t, 1)
	fixed := time.Date(2026, 9, 28, 1, 2, 3, 0, time.UTC)
	helper, err := NewEventHelper(claim, func() time.Time { return fixed })
	if err != nil {
		t.Fatal(err)
	}
	const count = 32
	sequences := make(chan uint64, count)
	var wait sync.WaitGroup
	for range count {
		wait.Add(1)
		go func() {
			defer wait.Done()
			metadata, err := helper.Next()
			if err != nil {
				t.Errorf("next: %v", err)
				return
			}
			if metadata.RunID != string(claim.RunID) || metadata.AttemptID != string(claim.AttemptID) || metadata.FencingToken != uint64(claim.FencingToken) || !metadata.OccurredAt.Equal(fixed) || !regexp.MustCompile(`^evt_[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`).MatchString(metadata.EventID) {
				t.Errorf("invalid metadata: %#v", metadata)
			}
			sequences <- metadata.ProducerSequence
		}()
	}
	wait.Wait()
	close(sequences)
	seen := map[uint64]bool{}
	for sequence := range sequences {
		seen[sequence] = true
	}
	for sequence := uint64(1); sequence <= count; sequence++ {
		if !seen[sequence] {
			t.Fatalf("missing producer sequence %d", sequence)
		}
	}
}

func TestSecureIdentifiersAreUUIDv7(t *testing.T) {
	now := time.Date(2026, 9, 28, 1, 2, 3, 0, time.UTC)
	session, err := NewSessionID(now)
	if err != nil || !regexp.MustCompile(`^ses_[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`).MatchString(session) {
		t.Fatalf("session=%q err=%v", session, err)
	}
	completion, key, err := (SecureCompletionSource{Clock: func() time.Time { return now }}).Completion(t.Context(), testClaim(t, 1))
	if err != nil || !regexp.MustCompile(`^cmp_[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`).MatchString(completion) || !validIdempotencyKey(key) {
		t.Fatalf("completion=%q key=%q err=%v", completion, key, err)
	}
}
