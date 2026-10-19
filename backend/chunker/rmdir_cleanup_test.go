package chunker

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/object"
	"github.com/stretchr/testify/require"
)

type rmdirCleanupTestFs struct {
	fs.Fs
	mu sync.Mutex

	listCalls      int
	failListCall   int
	listErr        error
	blockListCall  int
	listStarted    chan struct{}
	allowList      chan struct{}
	omitOnListCall map[int]map[string]bool
	removeErr      error
	putStarted     chan struct{}
	allowPut       chan struct{}
	putStartedOnce sync.Once
}

func (f *rmdirCleanupTestFs) List(ctx context.Context, dir string) (fs.DirEntries, error) {
	f.mu.Lock()
	f.listCalls++
	call := f.listCalls
	failList := call == f.failListCall
	blockList := call == f.blockListCall
	listErr := f.listErr
	listStarted, allowList := f.listStarted, f.allowList
	omit := f.omitOnListCall[call]
	removeErr := f.removeErr
	f.mu.Unlock()

	if blockList {
		select {
		case listStarted <- struct{}{}:
		default:
		}
		select {
		case <-allowList:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if failList {
		return nil, listErr
	}
	entries, err := f.Fs.List(ctx, dir)
	if err != nil {
		return nil, err
	}
	result := make(fs.DirEntries, 0, len(entries))
	for _, entry := range entries {
		if omit[entry.Remote()] {
			continue
		}
		if obj, ok := entry.(fs.Object); ok {
			entry = &rmdirCleanupTestObject{Object: obj, removeErr: removeErr}
		}
		result = append(result, entry)
	}
	return result, nil
}

func (f *rmdirCleanupTestFs) Put(ctx context.Context, in io.Reader, src fs.ObjectInfo, options ...fs.OpenOption) (fs.Object, error) {
	if f.putStarted != nil && strings.HasPrefix(src.Remote(), "dir/") {
		f.putStartedOnce.Do(func() {
			close(f.putStarted)
			select {
			case <-f.allowPut:
			case <-ctx.Done():
			}
		})
		if err := ctx.Err(); err != nil {
			return nil, err
		}
	}
	return f.Fs.Put(ctx, in, src, options...)
}

type rmdirCleanupTestObject struct {
	fs.Object
	removeErr error
}

func (o *rmdirCleanupTestObject) Remove(ctx context.Context) error {
	if o.removeErr != nil {
		return o.removeErr
	}
	return o.Object.Remove(ctx)
}

func newRmdirCleanupTestFs(t *testing.T, enabled bool) (*Fs, *rmdirCleanupTestFs, fs.Fs) {
	t.Helper()
	ctx := context.Background()
	base, err := fs.NewFs(ctx, t.TempDir())
	require.NoError(t, err)
	testBase := &rmdirCleanupTestFs{Fs: base}
	f := &Fs{
		base: testBase,
		opt: Options{
			ChunkSize:    4,
			StartFrom:    1,
			NameFormat:   "*.rclone_chunk.###",
			MetaFormat:   "simplejson",
			HashType:     "sha1all",
			Transactions: "rename",
			RmdirCleanup: enabled,
			FailHard:     true,
		},
	}
	f.dirSort = true
	require.NoError(t, f.configure(f.opt.NameFormat, f.opt.MetaFormat, f.opt.HashType, f.opt.Transactions))
	f.features = (&fs.Features{}).Fill(ctx, f).Mask(ctx, testBase).WrapsFs(f, testBase)
	require.NoError(t, base.Mkdir(ctx, "dir"))
	return f, testBase, base
}

func putRmdirCleanupOrphan(t *testing.T, base fs.Fs, remote string) {
	t.Helper()
	ctx := context.Background()
	contents := []byte("orphan chunk")
	info := object.NewStaticObjectInfo(remote, time.Now(), int64(len(contents)), true, nil, base)
	_, err := base.Put(ctx, bytes.NewReader(contents), info)
	require.NoError(t, err)
}

func requireObjectExists(t *testing.T, base fs.Fs, remote string) {
	t.Helper()
	_, err := base.NewObject(context.Background(), remote)
	require.NoError(t, err)
}

func TestRmdirCleanupRemovesOrphanChunksWhenEnabled(t *testing.T) {
	ctx := context.Background()
	f, _, base := newRmdirCleanupTestFs(t, true)
	orphan := f.makeChunkName("dir/file", 0, "", "")
	putRmdirCleanupOrphan(t, base, orphan)

	entries, err := f.List(ctx, "dir")
	require.NoError(t, err)
	require.Empty(t, entries, "orphan chunks are hidden from the logical listing")
	require.NoError(t, f.Rmdir(ctx, "dir"))

	_, err = base.NewObject(ctx, orphan)
	require.ErrorIs(t, err, fs.ErrorObjectNotFound)
}

func TestRmdirWithoutCleanupLeavesOrphanChunks(t *testing.T) {
	ctx := context.Background()
	f, _, base := newRmdirCleanupTestFs(t, false)
	orphan := f.makeChunkName("dir/file", 0, "", "")
	putRmdirCleanupOrphan(t, base, orphan)

	err := f.Rmdir(ctx, "dir")
	require.Error(t, err, "the default behavior must leave hidden chunks for the wrapped backend to reject")
	requireObjectExists(t, base, orphan)
}

func TestRmdirCleanupPreservesVisibleChildren(t *testing.T) {
	for _, tc := range []struct {
		name  string
		child func(t *testing.T, base fs.Fs)
	}{
		{
			name: "file",
			child: func(t *testing.T, base fs.Fs) {
				contents := []byte("visible")
				info := object.NewStaticObjectInfo("dir/visible", time.Now(), int64(len(contents)), true, nil, base)
				_, err := base.Put(context.Background(), bytes.NewReader(contents), info)
				require.NoError(t, err)
			},
		},
		{
			name: "directory",
			child: func(t *testing.T, base fs.Fs) {
				require.NoError(t, base.Mkdir(context.Background(), "dir/child"))
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			f, _, base := newRmdirCleanupTestFs(t, true)
			orphan := f.makeChunkName("dir/file", 0, "", "")
			putRmdirCleanupOrphan(t, base, orphan)
			tc.child(t, base)

			err := f.Rmdir(ctx, "dir")
			require.ErrorIs(t, err, fs.ErrorDirectoryNotEmpty)
			requireObjectExists(t, base, orphan)
		})
	}
}

func TestRmdirCleanupValidatesAllRawEntriesBeforeDeleting(t *testing.T) {
	ctx := context.Background()
	f, testBase, base := newRmdirCleanupTestFs(t, true)
	orphan := f.makeChunkName("dir/file", 0, "", "")
	putRmdirCleanupOrphan(t, base, orphan)
	contents := []byte("unknown raw object")
	info := object.NewStaticObjectInfo("dir/unknown", time.Now(), int64(len(contents)), true, nil, base)
	_, err := base.Put(ctx, bytes.NewReader(contents), info)
	require.NoError(t, err)
	testBase.omitOnListCall = map[int]map[string]bool{
		1: {"dir/unknown": true}, // Simulate a logical listing that omitted a raw child.
	}

	err = f.Rmdir(ctx, "dir")
	require.ErrorIs(t, err, fs.ErrorDirectoryNotEmpty)
	requireObjectExists(t, base, orphan)
	requireObjectExists(t, base, "dir/unknown")
}

func TestRmdirCleanupPropagatesListErrors(t *testing.T) {
	for _, failCall := range []int{1, 2} {
		t.Run(map[int]string{1: "logical", 2: "raw"}[failCall], func(t *testing.T) {
			ctx := context.Background()
			f, testBase, base := newRmdirCleanupTestFs(t, true)
			orphan := f.makeChunkName("dir/file", 0, "", "")
			putRmdirCleanupOrphan(t, base, orphan)
			listErr := errors.New("listing failed")
			testBase.failListCall = failCall
			testBase.listErr = listErr

			err := f.Rmdir(ctx, "dir")
			require.ErrorIs(t, err, listErr)
			requireObjectExists(t, base, orphan)
		})
	}
}

func TestRmdirCleanupPropagatesDeleteErrors(t *testing.T) {
	ctx := context.Background()
	f, testBase, base := newRmdirCleanupTestFs(t, true)
	orphan := f.makeChunkName("dir/file", 0, "", "")
	putRmdirCleanupOrphan(t, base, orphan)
	deleteErr := errors.New("delete failed")
	testBase.removeErr = deleteErr

	err := f.Rmdir(ctx, "dir")
	require.ErrorIs(t, err, deleteErr)
	requireObjectExists(t, base, orphan)
}

func TestRmdirCleanupRefusesConcurrentPut(t *testing.T) {
	ctx := context.Background()
	f, testBase, base := newRmdirCleanupTestFs(t, true)
	orphan := f.makeChunkName("dir/old", 0, "", "")
	putRmdirCleanupOrphan(t, base, orphan)
	testBase.putStarted = make(chan struct{})
	testBase.allowPut = make(chan struct{})
	putDone := make(chan error, 1)
	contents := []byte("concurrent upload")
	info := object.NewStaticObjectInfo("dir/upload", time.Now(), int64(len(contents)), true, nil, base)
	go func() {
		_, err := f.Put(ctx, bytes.NewReader(contents), info)
		putDone <- err
	}()
	select {
	case <-testBase.putStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("upload did not reach the wrapped Put")
	}

	err := f.Rmdir(ctx, "dir")
	require.ErrorIs(t, err, fs.ErrorDirectoryNotEmpty)
	requireObjectExists(t, base, orphan)
	close(testBase.allowPut)
	require.NoError(t, <-putDone)

	err = f.Rmdir(ctx, "dir")
	require.ErrorIs(t, err, fs.ErrorDirectoryNotEmpty)
}

func TestRmdirCleanupRejectsWritesWhileRemoving(t *testing.T) {
	ctx := context.Background()
	f, testBase, base := newRmdirCleanupTestFs(t, true)
	testBase.blockListCall = 1
	testBase.listStarted = make(chan struct{}, 1)
	testBase.allowList = make(chan struct{})
	rmdirDone := make(chan error, 1)
	go func() {
		rmdirDone <- f.Rmdir(ctx, "dir")
	}()
	select {
	case <-testBase.listStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("rmdir did not reach the wrapped listing")
	}

	contents := []byte("blocked upload")
	info := object.NewStaticObjectInfo("dir/upload", time.Now(), int64(len(contents)), true, nil, base)
	_, err := f.Put(ctx, bytes.NewReader(contents), info)
	require.ErrorIs(t, err, fs.ErrorDirectoryNotEmpty)
	close(testBase.allowList)
	require.NoError(t, <-rmdirDone)
}
