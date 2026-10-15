package degoo

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/object"
)

func TestMoveRejectsStaleObjectAfterUpdate(t *testing.T) {
	ctx := context.Background()
	mock := newMockDegoo()
	f, server := newTestFs(t, mock)
	defer server.Close()

	if err := f.Mkdir(ctx, "destination-dir"); err != nil {
		t.Fatalf("Mkdir destination: %v", err)
	}
	original := []byte("generated original content")
	if _, err := f.Put(ctx, bytes.NewReader(original), object.NewStaticObjectInfo("cipher", time.Now(), int64(len(original)), true, nil, f)); err != nil {
		t.Fatalf("Put original: %v", err)
	}
	stale, err := f.NewObject(ctx, "cipher")
	if err != nil {
		t.Fatalf("NewObject stale source: %v", err)
	}
	current, err := f.NewObject(ctx, "cipher")
	if err != nil {
		t.Fatalf("NewObject update source: %v", err)
	}
	replacement := []byte("generated replacement content")
	if err := current.Update(ctx, bytes.NewReader(replacement), object.NewStaticObjectInfo("cipher", time.Now(), int64(len(replacement)), true, nil, f)); err != nil {
		t.Fatalf("Update source: %v", err)
	}
	if stale.(*Object).ID() == current.(*Object).ID() {
		t.Fatalf("Update retained source ID %q; test requires an old handle", stale.(*Object).ID())
	}

	beforeMove := f.RequestStats()
	_, moveErr := f.Move(ctx, stale, "destination-dir/moved")
	if moveErr == nil {
		t.Fatal("Move with a stale source handle succeeded")
	}
	afterMove := f.RequestStats()
	for _, operation := range []string{"SetMoveFile", "SetRenameFile"} {
		if got := afterMove.RequestsByOperation[operation] - beforeMove.RequestsByOperation[operation]; got != 0 {
			t.Errorf("stale Move issued %s %d times; want no move mutation", operation, got)
		}
	}

	updated, err := f.NewObject(ctx, "cipher")
	if err != nil {
		t.Fatalf("replacement missing at original path: %v", err)
	}
	body, err := updated.Open(ctx)
	if err != nil {
		t.Fatalf("Open replacement at original path: %v", err)
	}
	got, readErr := io.ReadAll(body)
	closeTestBody(t, body)
	if readErr != nil || !bytes.Equal(got, replacement) {
		t.Fatalf("replacement at original path = %q, %v; want %q", got, readErr, replacement)
	}
	if _, err := f.NewObject(ctx, "destination-dir/moved"); !errors.Is(err, fs.ErrorObjectNotFound) {
		t.Fatalf("destination lookup = %v, want object not found", err)
	}
}
