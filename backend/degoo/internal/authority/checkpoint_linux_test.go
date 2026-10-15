//go:build linux

package authority

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestCheckpointInitializeLoadAndRestart(t *testing.T) {
	path := initializedAuthority(t)
	lease := openCheckpointLease(t, path)
	if payload, err := lease.LoadCheckpoint(); !errors.Is(err, ErrCheckpointMissing) || payload != nil {
		t.Fatalf("missing LoadCheckpoint = %q, %v; want nil, ErrCheckpointMissing", payload, err)
	}
	if _, err := os.Lstat(filepath.Join(path, checkpointName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("LoadCheckpoint created state: %v", err)
	}
	if err := lease.SaveCheckpoint([]byte("before load")); !errors.Is(err, ErrCheckpointNotLoaded) {
		t.Fatalf("SaveCheckpoint before positive load = %v, want ErrCheckpointNotLoaded", err)
	}
	want := []byte("initial control state")
	if err := lease.InitializeCheckpoint(want); err != nil {
		t.Fatalf("InitializeCheckpoint: %v", err)
	}
	if err := lease.InitializeCheckpoint([]byte("replacement")); !errors.Is(err, ErrCheckpointExists) {
		t.Fatalf("second InitializeCheckpoint = %v, want ErrCheckpointExists", err)
	}
	if got, err := lease.LoadCheckpoint(); err != nil || !bytes.Equal(got, want) {
		t.Fatalf("LoadCheckpoint = %q, %v; want %q", got, err, want)
	}
	checkCheckpointFile(t, filepath.Join(path, checkpointName), checkpointHeaderSize+len(want))
	if err := lease.SaveCheckpoint([]byte("replacement control state")); err != nil {
		t.Fatalf("SaveCheckpoint after load: %v", err)
	}
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}

	reopened := openCheckpointLease(t, path)
	if err := reopened.InitializeCheckpoint([]byte("must not replace existing state")); !errors.Is(err, ErrCheckpointExists) {
		t.Fatalf("InitializeCheckpoint after restart = %v, want ErrCheckpointExists", err)
	}
	if err := reopened.SaveCheckpoint([]byte("before restart load")); !errors.Is(err, ErrCheckpointNotLoaded) {
		t.Fatalf("SaveCheckpoint after Open but before Load = %v, want ErrCheckpointNotLoaded", err)
	}
	got, err := reopened.LoadCheckpoint()
	if err != nil || string(got) != "replacement control state" {
		t.Fatalf("reopened checkpoint = %q, %v", got, err)
	}
}

func TestCheckpointPayloadLimitAndOversizeDoesNotPoison(t *testing.T) {
	path := initializedAuthority(t)
	lease := openCheckpointLease(t, path)
	tooLarge := make([]byte, checkpointMaxPayload+1)
	if err := lease.InitializeCheckpoint(tooLarge); !errors.Is(err, ErrCheckpointTooLarge) {
		t.Fatalf("oversized initialization = %v, want ErrCheckpointTooLarge", err)
	}
	if err := lease.Check(); err != nil {
		t.Fatalf("oversize poisoned lease: %v", err)
	}
	want := bytes.Repeat([]byte{0x5a}, checkpointMaxPayload)
	if err := lease.InitializeCheckpoint(want); err != nil {
		t.Fatalf("initialize maximum payload: %v", err)
	}
	checkCheckpointFile(t, filepath.Join(path, checkpointName), checkpointMaxFile)
	if err := lease.SaveCheckpoint(tooLarge); !errors.Is(err, ErrCheckpointTooLarge) {
		t.Fatalf("oversized save = %v, want ErrCheckpointTooLarge", err)
	}
	if err := lease.Check(); err != nil {
		t.Fatalf("oversize save poisoned lease: %v", err)
	}
	got, err := lease.LoadCheckpoint()
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("checkpoint after oversized save = %d bytes, %v", len(got), err)
	}
}

func TestCheckpointRefusesChangedStateBeforeSave(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(t *testing.T, path string)
	}{
		{
			name: "content",
			change: func(t *testing.T, path string) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(path, checkpointName), encodeCheckpoint([]byte("other state")), fileMode); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "inode",
			change: func(t *testing.T, path string) {
				t.Helper()
				replacement := filepath.Join(path, "replacement")
				if err := os.WriteFile(replacement, encodeCheckpoint([]byte("original state")), fileMode); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(replacement, fileMode); err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(replacement, filepath.Join(path, checkpointName)); err != nil {
					t.Fatal(err)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := initializedAuthority(t)
			lease := openCheckpointLease(t, path)
			if err := lease.InitializeCheckpoint([]byte("original state")); err != nil {
				t.Fatal(err)
			}
			tc.change(t, path)
			if err := lease.SaveCheckpoint([]byte("must not replace changed state")); !errors.Is(err, ErrCheckpointChanged) {
				t.Fatalf("SaveCheckpoint after external change = %v, want ErrCheckpointChanged", err)
			}
			if err := lease.Check(); !errors.Is(err, ErrCheckpointPoisoned) {
				t.Fatalf("Check after changed state = %v, want ErrCheckpointPoisoned", err)
			}
			got, err := os.ReadFile(filepath.Join(path, checkpointName))
			if err != nil {
				t.Fatal(err)
			}
			if bytes.Equal(got, encodeCheckpoint([]byte("must not replace changed state"))) {
				t.Fatal("SaveCheckpoint overwrote changed state")
			}
		})
	}
}

func TestCheckpointCheckRejectsCorruptionAndUnsafeFiles(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(t *testing.T, path string)
		want   error
	}{
		{
			name: "bad checksum",
			change: func(t *testing.T, path string) {
				t.Helper()
				raw := encodeCheckpoint([]byte("state"))
				raw[len(raw)-1] ^= 0xff
				replaceCheckpoint(t, path, raw)
			},
			want: ErrCheckpointCorrupt,
		},
		{
			name: "truncated envelope",
			change: func(t *testing.T, path string) {
				t.Helper()
				replaceCheckpoint(t, path, encodeCheckpoint([]byte("state"))[:checkpointHeaderSize-1])
			},
			want: ErrCheckpointCorrupt,
		},
		{
			name: "trailing data",
			change: func(t *testing.T, path string) {
				t.Helper()
				replaceCheckpoint(t, path, append(encodeCheckpoint([]byte("state")), 0))
			},
			want: ErrCheckpointCorrupt,
		},
		{
			name: "oversized file",
			change: func(t *testing.T, path string) {
				t.Helper()
				replaceCheckpoint(t, path, make([]byte, checkpointMaxFile+1))
			},
			want: ErrCheckpointTooLarge,
		},
		{
			name: "symlink",
			change: func(t *testing.T, path string) {
				t.Helper()
				state := filepath.Join(path, checkpointName)
				target := filepath.Join(path, "outside")
				if err := os.WriteFile(target, encodeCheckpoint([]byte("state")), fileMode); err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(state); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, state); err != nil {
					t.Fatal(err)
				}
			},
			want: ErrCheckpointUnsafe,
		},
		{
			name: "permissions",
			change: func(t *testing.T, path string) {
				t.Helper()
				if err := os.Chmod(filepath.Join(path, checkpointName), 0640); err != nil {
					t.Fatal(err)
				}
			},
			want: ErrCheckpointUnsafe,
		},
		{
			name: "hard link",
			change: func(t *testing.T, path string) {
				t.Helper()
				if err := os.Link(filepath.Join(path, checkpointName), filepath.Join(path, "outside-link")); err != nil {
					t.Fatal(err)
				}
			},
			want: ErrCheckpointUnsafe,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := initializedAuthority(t)
			lease := openCheckpointLease(t, path)
			if err := lease.InitializeCheckpoint([]byte("state")); err != nil {
				t.Fatal(err)
			}
			tc.change(t, path)
			if err := lease.Check(); !errors.Is(err, tc.want) {
				t.Fatalf("Check after change = %v, want %v", err, tc.want)
			}
			if err := lease.Check(); !errors.Is(err, ErrCheckpointPoisoned) {
				t.Fatalf("second Check = %v, want ErrCheckpointPoisoned", err)
			}
		})
	}
}

func TestCheckpointRecoversOnlyFixedTemporaryFile(t *testing.T) {
	t.Run("valid state and leftover temp", func(t *testing.T) {
		path := initializedAuthority(t)
		lease := openCheckpointLease(t, path)
		if err := lease.InitializeCheckpoint([]byte("committed")); err != nil {
			t.Fatal(err)
		}
		if err := lease.Close(); err != nil {
			t.Fatal(err)
		}
		writePrivateFile(t, filepath.Join(path, checkpointTempName), []byte("uncommitted partial bytes"))
		other := filepath.Join(path, "control.tmp.keep")
		writePrivateFile(t, other, []byte("unrelated"))

		reopened := openCheckpointLease(t, path)
		got, err := reopened.LoadCheckpoint()
		if err != nil || string(got) != "committed" {
			t.Fatalf("LoadCheckpoint = %q, %v", got, err)
		}
		if _, err := os.Lstat(filepath.Join(path, checkpointTempName)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("fixed temporary file was not recovered: %v", err)
		}
		if _, err := os.Stat(other); err != nil {
			t.Fatalf("recovery removed unrelated file: %v", err)
		}
	})
	t.Run("missing main remains missing", func(t *testing.T) {
		path := initializedAuthority(t)
		writePrivateFile(t, filepath.Join(path, checkpointTempName), []byte("uncommitted partial bytes"))
		lease := openCheckpointLease(t, path)
		if _, err := lease.LoadCheckpoint(); !errors.Is(err, ErrCheckpointMissing) {
			t.Fatalf("LoadCheckpoint = %v, want ErrCheckpointMissing", err)
		}
		if _, err := os.Lstat(filepath.Join(path, checkpointName)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("LoadCheckpoint created main state: %v", err)
		}
		if _, err := os.Lstat(filepath.Join(path, checkpointTempName)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("LoadCheckpoint left uncommitted temporary file: %v", err)
		}
		if err := lease.InitializeCheckpoint([]byte("explicit")); err != nil {
			t.Fatalf("explicit initialization after missing load: %v", err)
		}
	})
	t.Run("unsafe temp is not removed", func(t *testing.T) {
		path := initializedAuthority(t)
		target := filepath.Join(path, "outside")
		writePrivateFile(t, target, []byte("keep"))
		if err := os.Symlink(target, filepath.Join(path, checkpointTempName)); err != nil {
			t.Fatal(err)
		}
		lease := openCheckpointLease(t, path)
		if _, err := lease.LoadCheckpoint(); !errors.Is(err, ErrCheckpointUnsafe) {
			t.Fatalf("LoadCheckpoint with symlink temp = %v, want ErrCheckpointUnsafe", err)
		}
		if _, err := os.Lstat(filepath.Join(path, checkpointTempName)); err != nil {
			t.Fatalf("recovery removed unsafe temporary entry: %v", err)
		}
	})
}

func TestCheckpointPostRenameFailurePoisonsUntilReopen(t *testing.T) {
	path := initializedAuthority(t)
	lease := openCheckpointLease(t, path)
	if err := lease.InitializeCheckpoint([]byte("old")); err != nil {
		t.Fatal(err)
	}
	injected := errors.New("injected directory-sync failure")
	err := lease.saveCheckpoint([]byte("new committed bytes"), func() error { return injected })
	if !errors.Is(err, ErrCheckpointIO) || !errors.Is(err, injected) {
		t.Fatalf("post-rename save error = %v, want checkpoint I/O and injected error", err)
	}
	if err := lease.Check(); !errors.Is(err, ErrCheckpointPoisoned) {
		t.Fatalf("Check after ambiguous save = %v, want ErrCheckpointPoisoned", err)
	}
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}

	reopened := openCheckpointLease(t, path)
	got, err := reopened.LoadCheckpoint()
	if err != nil || string(got) != "new committed bytes" {
		t.Fatalf("reopened checkpoint after post-rename error = %q, %v", got, err)
	}
}

func TestCheckpointPreRenameFailurePreservesMainState(t *testing.T) {
	path := initializedAuthority(t)
	lease := openCheckpointLease(t, path)
	if err := lease.InitializeCheckpoint([]byte("still committed")); err != nil {
		t.Fatal(err)
	}
	mainBefore, err := os.ReadFile(filepath.Join(path, checkpointName))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("unrelated", filepath.Join(path, checkpointTempName)); err != nil {
		t.Fatal(err)
	}
	if err := lease.SaveCheckpoint([]byte("must not publish")); !errors.Is(err, ErrCheckpointUnsafe) {
		t.Fatalf("SaveCheckpoint with unsafe temp = %v, want ErrCheckpointUnsafe", err)
	}
	mainAfter, err := os.ReadFile(filepath.Join(path, checkpointName))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(mainAfter, mainBefore) {
		t.Fatal("pre-rename failure changed committed checkpoint")
	}
	if err := lease.Check(); !errors.Is(err, ErrCheckpointPoisoned) {
		t.Fatalf("Check after unsafe temp = %v, want ErrCheckpointPoisoned", err)
	}
}

func TestCheckpointSaveAndCloseAreSerialized(t *testing.T) {
	path := initializedAuthority(t)
	lease := openCheckpointLease(t, path)
	if err := lease.InitializeCheckpoint([]byte("before")); err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	saveDone := make(chan error, 1)
	go func() {
		saveDone <- lease.saveCheckpoint([]byte("after"), func() error {
			close(started)
			<-release
			return nil
		})
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("save did not reach post-rename hook")
	}
	closeStarted := make(chan struct{})
	closeDone := make(chan error, 1)
	go func() {
		close(closeStarted)
		closeDone <- lease.Close()
	}()
	<-closeStarted
	select {
	case err := <-closeDone:
		t.Fatalf("Close completed during serialized save: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	unblock()
	if err := <-saveDone; err != nil {
		t.Fatalf("SaveCheckpoint: %v", err)
	}
	if err := <-closeDone; err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := lease.Check(); !errors.Is(err, ErrClosed) {
		t.Fatalf("Check after Close = %v, want ErrClosed", err)
	}
}

func openCheckpointLease(t *testing.T, path string) *Lease {
	t.Helper()
	lease, err := Open(context.Background(), path, testVaultIdentity)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := lease.Close(); err != nil {
			t.Errorf("Close checkpoint lease: %v", err)
		}
	})
	return lease
}

func checkCheckpointFile(t *testing.T, path string, size int) {
	t.Helper()
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer closeFD(fd)
	st, err := statFD(fd)
	if err != nil {
		t.Fatal(err)
	}
	if err := requirePrivateFile(&st, uint32(os.Geteuid())); err != nil {
		t.Fatalf("checkpoint file is not private: %v", err)
	}
	if st.Size != int64(size) {
		t.Fatalf("checkpoint size = %d, want %d", st.Size, size)
	}
}

func replaceCheckpoint(t *testing.T, path string, contents []byte) {
	t.Helper()
	state := filepath.Join(path, checkpointName)
	if err := os.WriteFile(state, contents, fileMode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(state, fileMode); err != nil {
		t.Fatal(err)
	}
}

func writePrivateFile(t *testing.T, path string, contents []byte) {
	t.Helper()
	if err := os.WriteFile(path, contents, fileMode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, fileMode); err != nil {
		t.Fatal(err)
	}
}
