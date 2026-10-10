//go:build linux

package degoo

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rclone/rclone/backend/degoo/auth"
	_ "github.com/rclone/rclone/backend/local"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/config"
	"github.com/rclone/rclone/fs/object"
	"github.com/rclone/rclone/vfs"
	"github.com/rclone/rclone/vfs/vfscommon"
	"github.com/stretchr/testify/require"
)

func testConfig(t *testing.T) *VaultConfig {
	cacheDir := filepath.Join(t.TempDir(), "cache")
	require.NoError(t, os.Mkdir(cacheDir, 0700))
	return &VaultConfig{Version: 1, Remote: "TestDegoo-" + strings.ReplaceAll(t.Name(), "/", "-") + ":", Password: "generated vault password", CacheDir: cacheDir, ChunkSize: 1024, MaxObjectSize: 4096}
}

func localStack(t *testing.T, cfg *VaultConfig) (fs.Fs, fs.Fs) {
	t.Helper()
	base, err := fs.NewFs(context.Background(), t.TempDir())
	require.NoError(t, err)
	stack, err := newStack(context.Background(), base, cfg)
	require.NoError(t, err)
	return base, stack
}

func put(t *testing.T, f fs.Fs, name string, data []byte) fs.Object {
	t.Helper()
	info := object.NewStaticObjectInfo(name, time.Now(), int64(len(data)), true, nil, f)
	obj, err := f.Put(context.Background(), bytes.NewReader(data), info)
	require.NoError(t, err)
	return obj
}

func read(t *testing.T, obj fs.Object, options ...fs.OpenOption) []byte {
	t.Helper()
	r, err := obj.Open(context.Background(), options...)
	require.NoError(t, err)
	got, err := io.ReadAll(r)
	require.NoError(t, err)
	require.NoError(t, r.Close())
	return got
}

func TestVaultConfigurationAndCacheProtection(t *testing.T) {
	ctx := context.Background()
	cfg := testConfig(t)
	require.NoError(t, cfg.validate())
	filename := filepath.Join(t.TempDir(), "private", "vault.json")
	data, err := json.Marshal(cfg)
	require.NoError(t, err)
	require.NoError(t, auth.WritePrivateFile(filename, data))
	got, err := readConfig(filename)
	require.NoError(t, err)
	require.Equal(t, cfg, got)
	require.NoError(t, os.Chmod(filename, 0644))
	_, err = readConfig(filename)
	require.Error(t, err)
	bad := *cfg
	bad.Password = ""
	require.Error(t, bad.validate())
	bad = *cfg
	bad.ChunkSize = bad.MaxObjectSize
	require.Error(t, bad.validate(), "ciphertext overhead counts against object cap")
	bad = *cfg
	bad.Remote = ":local:/tmp"
	require.Error(t, bad.validate())
	bad = *cfg
	bad.Version = 2
	require.Error(t, bad.validate())
	ctx, ci := fs.AddConfig(ctx)
	ci.Dump = fs.DumpBodies
	require.Error(t, safeLogging(ctx))

	lock, err := lockCache(cfg)
	require.NoError(t, err)
	_, err = lockCache(cfg)
	require.Error(t, err)
	require.NoError(t, lock.Close())
	lock, err = lockCache(cfg)
	require.NoError(t, err)
	require.NoError(t, lock.Close())
	base, _ := localStack(t, cfg)
	require.NoError(t, bindCache(cfg, base))
	require.NoError(t, bindCache(cfg, base))
	bad = *cfg
	bad.Password = "another password"
	require.Error(t, bindCache(&bad, base), "pending plaintext cannot be silently moved to a different vault")
	metadataDir := filepath.Join(cfg.CacheDir, "vfsMeta", cfg.cacheName())
	require.NoError(t, os.MkdirAll(metadataDir, 0700))
	require.NoError(t, os.WriteFile(filepath.Join(metadataDir, "pending"), []byte(`{"Dirty":true,"Size":1}`), 0600))
	require.NoError(t, os.Remove(filepath.Join(cfg.CacheDir, cfg.cacheName()+".identity")))
	require.Error(t, bindCache(&bad, base), "missing identity must not allow pending plaintext to be rebound")
}

func TestVaultEncryptsChunksNamesAndManifests(t *testing.T) {
	ctx := context.Background()
	cfg := testConfig(t)
	base, stack := localStack(t, cfg)
	data := bytes.Repeat([]byte("generated plaintext contents "), 200)
	obj := put(t, stack, "private-dir/private-file.txt", data)
	require.Equal(t, data, read(t, obj))
	require.Equal(t, data[950:2150], read(t, obj, &fs.RangeOption{Start: 950, End: 2149}))
	root, err := base.List(ctx, "")
	require.NoError(t, err)
	require.Len(t, root, 1)
	require.NotContains(t, root[0].Remote(), "private")
	entries, err := base.List(ctx, root[0].Remote())
	require.NoError(t, err)
	require.Len(t, entries, (len(data)+1023)/1024+1)
	for _, entry := range entries {
		require.NotContains(t, entry.Remote(), "private")
		require.NotContains(t, entry.Remote(), "rclone_chunk")
		encoded := read(t, entry.(fs.Object))
		require.True(t, bytes.HasPrefix(encoded, []byte("RCLONE")), "data and manifest both use the crypt format")
		require.NotContains(t, string(encoded), "generated plaintext contents")
		require.NotContains(t, string(encoded), "nchunks")
		require.LessOrEqual(t, int64(len(encoded)), int64(cfg.MaxObjectSize))
	}

	// A fresh overlay must recover the mapping entirely from encrypted metadata.
	remounted, err := newStack(ctx, base, cfg)
	require.NoError(t, err)
	obj, err = remounted.NewObject(ctx, "private-dir/private-file.txt")
	require.NoError(t, err)
	require.Equal(t, data, read(t, obj))
	moved, err := remounted.Features().Move(ctx, obj, "private-dir/renamed.txt")
	require.NoError(t, err)
	require.Equal(t, data, read(t, moved))
	require.NoError(t, moved.Remove(ctx))
	require.NoError(t, remounted.Rmdir(ctx, "private-dir"))
}

func TestVaultRejectsWrongPasswordAndDamagedChunks(t *testing.T) {
	for _, damage := range []string{"corrupt", "missing", "wrong-password"} {
		t.Run(damage, func(t *testing.T) {
			ctx := context.Background()
			cfg := testConfig(t)
			base, stack := localStack(t, cfg)
			data := bytes.Repeat([]byte("generated data "), 500)
			put(t, stack, "test", data)
			if damage == "wrong-password" {
				cfg.Password = "wrong vault secret"
				wrong, err := newStack(ctx, base, cfg)
				require.NoError(t, err)
				_, err = wrong.NewObject(ctx, "test")
				require.Error(t, err)
				return
			}
			entries, err := base.List(ctx, "")
			require.NoError(t, err)
			var chunk fs.Object
			for _, entry := range entries {
				candidate := entry.(fs.Object)
				if candidate.Size() > 1000 {
					chunk = candidate
					break
				}
			}
			require.NotNil(t, chunk)
			if damage == "missing" {
				require.NoError(t, chunk.Remove(ctx))
			} else {
				ciphertext := read(t, chunk)
				ciphertext[len(ciphertext)-1] ^= 1
				put(t, base, chunk.Remote(), ciphertext)
			}
			fresh, err := newStack(ctx, base, cfg)
			require.NoError(t, err)
			obj, err := fresh.NewObject(ctx, "test")
			if err == nil {
				r, openErr := obj.Open(ctx)
				if openErr == nil {
					_, err = io.ReadAll(r)
					require.NoError(t, r.Close())
				} else {
					err = openErr
				}
			}
			require.Error(t, err, "damage must fail the whole read")
		})
	}
}

func TestVaultPendingWriteSurvivesRestart(t *testing.T) {
	ctx := context.Background()
	cfg := testConfig(t)
	base, stack := localStack(t, cfg)
	oldCache := config.GetCacheDir()
	require.NoError(t, config.SetCacheDir(cfg.CacheDir))
	t.Cleanup(func() { require.NoError(t, config.SetCacheDir(oldCache)) })
	options := vfscommon.Opt
	options.CacheMode = vfscommon.CacheModeFull
	options.WriteBack = fs.Duration(time.Hour)
	options.PollInterval = 0
	v := vfs.New(ctx, stack, &options)
	fd, err := v.OpenFile("pending-file", os.O_CREATE|os.O_RDWR, 0600)
	require.NoError(t, err)
	data := []byte("generated locally saved write")
	_, err = fd.Write(data)
	require.NoError(t, err)
	require.NoError(t, fd.Sync())
	require.NoError(t, fd.Close())
	files, size, err := pending(cfg)
	require.NoError(t, err)
	require.Equal(t, int64(1), files)
	require.Equal(t, int64(len(data)), size)
	_, err = base.NewObject(ctx, "pending-file")
	require.ErrorIs(t, err, fs.ErrorObjectNotFound)
	v.Shutdown()

	stack, err = newStack(ctx, base, cfg)
	require.NoError(t, err)
	options.WriteBack = fs.Duration(20 * time.Millisecond)
	v = vfs.New(ctx, stack, &options)
	defer v.Shutdown()
	require.Eventually(t, func() bool {
		obj, err := stack.NewObject(ctx, "pending-file")
		if err != nil {
			return false
		}
		r, err := obj.Open(ctx)
		if err != nil {
			return false
		}
		got, readErr := io.ReadAll(r)
		_ = r.Close()
		return readErr == nil && bytes.Equal(got, data)
	}, 10*time.Second, 20*time.Millisecond)
	// Remote publication precedes the asynchronous clean-cache metadata update.
	require.Eventually(t, func() bool {
		files, _, err := pending(cfg)
		return err == nil && files == 0
	}, 10*time.Second, 20*time.Millisecond)
}
