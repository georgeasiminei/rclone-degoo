package chunkedreader

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math/rand"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/asyncreader"
	"github.com/rclone/rclone/fs/fserrors"
	"github.com/rclone/rclone/fstest/mockobject"
	"github.com/rclone/rclone/lib/multipart"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParallel(t *testing.T) {
	content := makeContent(t, 1024)

	for _, mode := range mockobject.SeekModes {
		t.Run(mode.String(), testRead(content, mode, 3))
	}
}

type openErrorObject struct {
	*mockobject.ContentMockObject
	err   error
	opens atomic.Int32
}

func (o *openErrorObject) Open(context.Context, ...fs.OpenOption) (io.ReadCloser, error) {
	o.opens.Add(1)
	return nil, o.err
}

func TestParallelOpenError(t *testing.T) {
	const size = 2 * multipart.BufferSize
	wantErr := errors.New("simulated HTTP 429")
	o := &openErrorObject{
		ContentMockObject: mockobject.New("test.bin").WithContent(make([]byte, size), mockobject.SeekModeNone),
		err:               wantErr,
	}
	cr := New(context.Background(), o, multipart.BufferSize, 0, 2)

	got := make(chan error, 1)
	go func() {
		buf := make([]byte, 1)
		_, err := cr.Read(buf)
		got <- err
	}()

	select {
	case err := <-got:
		if !errors.Is(err, wantErr) {
			t.Fatalf("Read() error = %v, want %v", err, wantErr)
		}
	case <-time.After(250 * time.Millisecond):
		t.Fatalf("Read() did not return after all stream opens failed (opens=%d)", o.opens.Load())
	}
}

type customOpenObject struct {
	*mockobject.ContentMockObject
	openFn func(context.Context, ...fs.OpenOption) (io.ReadCloser, error)
}

func (o *customOpenObject) Open(ctx context.Context, options ...fs.OpenOption) (io.ReadCloser, error) {
	return o.openFn(ctx, options...)
}

type errorAfterReader struct {
	payload []byte
	err     error
	done    bool
}

func (r *errorAfterReader) Read(p []byte) (int, error) {
	if r.done {
		return 0, r.err
	}
	r.done = true
	return copy(p, r.payload), r.err
}

func (r *errorAfterReader) Close() error { return nil }

func TestParallelReadErrorPreservesBufferedBytes(t *testing.T) {
	const size = 2 * multipart.BufferSize
	wantErr := errors.New("simulated HTTP 429 while reading")
	payload := []byte("received before the server error")
	o := &customOpenObject{
		ContentMockObject: mockobject.New("test.bin").WithContent(make([]byte, size), mockobject.SeekModeNone),
		openFn: func(context.Context, ...fs.OpenOption) (io.ReadCloser, error) {
			return &errorAfterReader{payload: payload, err: fserrors.NoLowLevelRetryError(wantErr)}, nil
		},
	}
	cr := New(context.Background(), o, multipart.BufferSize, 0, 2)
	buf := make([]byte, len(payload)+1)

	n, err := cr.Read(buf)
	assert.Equal(t, payload, buf[:n])
	assert.Equal(t, len(payload), n)
	assert.ErrorIs(t, err, wantErr)
	assert.ErrorIs(t, cr.Close(), wantErr)
}

func TestParallelUnexpectedEOF(t *testing.T) {
	const size = 2 * multipart.BufferSize
	payload := []byte("short successful response")
	o := &customOpenObject{
		ContentMockObject: mockobject.New("test.bin").WithContent(make([]byte, size), mockobject.SeekModeNone),
		openFn: func(context.Context, ...fs.OpenOption) (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader(payload)), nil
		},
	}
	cr := New(context.Background(), o, multipart.BufferSize, 0, 2)
	buf := make([]byte, len(payload)+1)

	n, err := cr.Read(buf)
	assert.Equal(t, payload, buf[:n])
	assert.Equal(t, len(payload), n)
	assert.ErrorIs(t, err, io.ErrUnexpectedEOF)
	assert.NoError(t, cr.Close())
}

func TestParallelReadCancellationAndClose(t *testing.T) {
	const size = 2 * multipart.BufferSize
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{}, 1)
	o := &customOpenObject{
		ContentMockObject: mockobject.New("test.bin").WithContent(make([]byte, size), mockobject.SeekModeNone),
		openFn: func(ctx context.Context, _ ...fs.OpenOption) (io.ReadCloser, error) {
			select {
			case started <- struct{}{}:
			default:
			}
			<-ctx.Done()
			return nil, ctx.Err()
		},
	}
	cr := New(ctx, o, multipart.BufferSize, 0, 2)

	readDone := make(chan error, 1)
	go func() {
		_, err := cr.Read(make([]byte, 1))
		readDone <- err
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("stream Open was not started")
	}
	cancel()

	closeDone := make(chan error, 1)
	go func() { closeDone <- cr.Close() }()
	select {
	case err := <-readDone:
		assert.ErrorIs(t, err, context.Canceled)
	case <-time.After(time.Second):
		t.Fatal("Read() did not return after its context was canceled")
	}
	select {
	case err := <-closeDone:
		assert.ErrorIs(t, err, asyncreader.ErrorStreamAbandoned)
	case <-time.After(time.Second):
		t.Fatal("Close() did not finish after the canceled stream stopped")
	}
}

func TestParallelErrorAfterClose(t *testing.T) {
	testErrorAfterClose(t, 3)
}

func TestParallelLarge(t *testing.T) {
	ctx := context.Background()
	const streams = 3
	const chunkSize = multipart.BufferSize
	const size = (2*streams+1)*chunkSize + 255
	content := makeContent(t, size)
	o := mockobject.New("test.bin").WithContent(content, mockobject.SeekModeNone)

	cr := New(ctx, o, chunkSize, 0, streams)

	for _, test := range []struct {
		name     string
		offset   int64
		seekMode int
	}{
		{name: "Straight", offset: 0, seekMode: -1},
		{name: "Rewind", offset: 0, seekMode: io.SeekStart},
		{name: "NearStart", offset: 1, seekMode: io.SeekStart},
		{name: "NearEnd", offset: size - 2*chunkSize - 127, seekMode: io.SeekEnd},
	} {
		t.Run(test.name, func(t *testing.T) {
			if test.seekMode >= 0 {
				var n int64
				var err error
				if test.seekMode == io.SeekEnd {
					n, err = cr.Seek(test.offset-size, test.seekMode)
				} else {
					n, err = cr.Seek(test.offset, test.seekMode)
				}
				require.NoError(t, err)
				assert.Equal(t, test.offset, n)
			}
			got, err := io.ReadAll(cr)
			require.NoError(t, err)
			require.Equal(t, len(content[test.offset:]), len(got))
			assert.Equal(t, content[test.offset:], got)
		})
	}

	require.NoError(t, cr.Close())

	t.Run("Seeky", func(t *testing.T) {
		cr := New(ctx, o, chunkSize, 0, streams)
		offset := 0
		buf := make([]byte, 1024)

		for {
			// Read and check a random read
			readSize := rand.Intn(1024)
			readBuf := buf[:readSize]
			n, err := cr.Read(readBuf)

			require.Equal(t, content[offset:offset+n], readBuf[:n])
			offset += n

			if err == io.EOF {
				assert.Equal(t, size, offset)
				break
			}
			require.NoError(t, err)

			// Now do a smaller random seek backwards
			seekSize := rand.Intn(512)
			if offset-seekSize < 0 {
				seekSize = offset
			}
			nn, err := cr.Seek(-int64(seekSize), io.SeekCurrent)
			offset -= seekSize
			require.NoError(t, err)
			assert.Equal(t, nn, int64(offset))
		}

		require.NoError(t, cr.Close())
	})

}
