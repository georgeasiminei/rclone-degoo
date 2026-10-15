package degoo

import (
	"bytes"
	"context"
	"io"
	"testing"
	"time"

	"github.com/rclone/rclone/fs/object"
)

func TestUpdateRetainsPublishedObjectIdentity(t *testing.T) {
	ctx := context.Background()
	mock := newMockDegoo()
	mock.items["generated-original"] = mockItem{id: "generated-original", name: "generated-file", parentID: "vault", category: 6, size: 3, content: []byte("old")}
	f, server := newTestFs(t, mock)
	defer server.Close()
	obj, err := f.NewObject(ctx, "generated-file")
	if err != nil {
		t.Fatal(err)
	}
	for _, data := range [][]byte{[]byte("generated replacement"), []byte("second generated replacement")} {
		oldID := obj.(*Object).ID()
		if err := obj.Update(ctx, bytes.NewReader(data), object.NewStaticObjectInfo(obj.Remote(), time.Now(), int64(len(data)), true, nil, f)); err != nil {
			t.Fatal(err)
		}
		published, err := f.NewObject(ctx, obj.Remote())
		if err != nil {
			t.Fatal(err)
		}
		if obj.(*Object).ID() == oldID || obj.(*Object).ID() != published.(*Object).ID() || obj.Size() != int64(len(data)) {
			t.Fatalf("updated receiver identity/size = %q/%d, published = %q/%d", obj.(*Object).ID(), obj.Size(), published.(*Object).ID(), published.Size())
		}
		body, err := obj.Open(ctx)
		if err != nil {
			t.Fatalf("open updated receiver: %v", err)
		}
		got, readErr := io.ReadAll(body)
		closeTestBody(t, body)
		if readErr != nil {
			t.Fatal(readErr)
		}
		if !bytes.Equal(got, data) {
			t.Fatal("updated receiver returned old or mixed data")
		}
	}
}

func TestFailedUpdatePreservesObjectIdentity(t *testing.T) {
	ctx := context.Background()
	mock := newMockDegoo()
	mock.items["generated-original"] = mockItem{
		id: "generated-original", name: "generated-file", parentID: "vault", category: 6,
		size: 3, content: []byte("old"),
	}
	mock.corruptUpload = true
	f, server := newTestFs(t, mock)
	defer server.Close()
	obj, err := f.NewObject(ctx, "generated-file")
	if err != nil {
		t.Fatal(err)
	}
	original := obj.(*Object).snapshot()
	data := []byte("generated replacement")
	if err := obj.Update(ctx, bytes.NewReader(data), object.NewStaticObjectInfo(obj.Remote(), time.Now(), int64(len(data)), true, nil, f)); err == nil {
		t.Fatal("update with corrupt upload succeeded")
	}
	if got := obj.(*Object).snapshot(); got.id != original.id || got.parentID != original.parentID || got.name != original.name ||
		got.size != original.size || !got.modTime.Equal(original.modTime) || !got.createdTime.Equal(original.createdTime) ||
		got.url != original.url || !got.urlAt.Equal(original.urlAt) {
		t.Fatal("failed update changed the receiver's published metadata")
	}
	body, err := obj.Open(ctx)
	if err != nil {
		t.Fatalf("open original receiver after failed update: %v", err)
	}
	got, readErr := io.ReadAll(body)
	closeTestBody(t, body)
	if readErr != nil || !bytes.Equal(got, []byte("old")) {
		t.Fatalf("original receiver after failed update: bytes=%q err=%v", got, readErr)
	}
}
