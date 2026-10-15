package api

import (
	"context"
	"testing"
	"time"
)

func TestFreshListingRecordsInvalidationBeforeCompletion(t *testing.T) {
	client, _ := newScalabilityOccupancyClient(t, 1)
	key := metadataFlightKey{parentID: "generated-parent", fresh: true}
	client.metadataMu.Lock()
	call := client.newMetadataListCallLocked(context.Background(), key)
	client.metadataMu.Unlock()

	cleanupStarted := make(chan struct{})
	allowCleanup := make(chan struct{})
	finished := make(chan struct{})
	cancel := call.cancel
	// Hold post-completion cleanup so the counter cannot catch up before the
	// caller observes the completed listing.
	call.cancel = func() {
		close(cleanupStarted)
		<-allowCleanup
		cancel()
	}
	t.Cleanup(func() {
		close(allowCleanup)
		<-finished
	})
	go func() {
		defer close(finished)
		client.runMetadataListCall(key, call)
	}()
	select {
	case <-cleanupStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("listing did not reach post-completion cleanup")
	}
	select {
	case <-call.done:
	default:
		t.Fatal("listing has not completed")
	}
	if call.err != nil || len(call.items) != 1 {
		t.Fatalf("listing result: %d items, %v", len(call.items), call.err)
	}
	if got := client.RequestStats().MetadataParentInvalidations; got != 1 {
		t.Fatalf("completed fresh-list end invalidations = %d, want 1", got)
	}
}
