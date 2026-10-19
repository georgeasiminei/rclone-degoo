package degoo

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/rclone/rclone/backend/degoo/auth"
	"github.com/rclone/rclone/fs"
)

const (
	metadataCacheVersion       = 1
	metadataCacheMaxObjectSize = 256 << 10
	metadataCacheMaxBytes      = 64 << 20
	metadataCacheMaxEntries    = 8192
	metadataCacheTTL           = 30 * time.Minute
	metadataCacheFillTimeout   = 10 * time.Second
	metadataCacheSweepInterval = time.Minute
)

type metadataCacheHeader struct {
	Version int       `json:"version"`
	Key     string    `json:"key_sha256"`
	Size    int64     `json:"size"`
	Stored  time.Time `json:"stored_at"`
	SHA256  string    `json:"content_sha256"`
}

type metadataCacheFlight struct {
	done chan struct{}
	data []byte
	err  error
}

type metadataCache struct {
	dir string

	mu         sync.Mutex
	flights    map[string]*metadataCacheFlight
	stats      MetadataCacheStats
	now        func() time.Time
	generation uint64
	disabled   bool
	entries    map[string]metadataCacheEntry
	totalBytes int64
	dirModTime time.Time
	lastSweep  time.Time
}

// MetadataCacheStats contains counters that do not expose cache paths or item
// metadata.
type MetadataCacheStats struct {
	Hits      uint64 `json:"hits"`
	Misses    uint64 `json:"misses"`
	Fills     uint64 `json:"fills"`
	Evictions uint64 `json:"evictions"`
}

// EnableMetadataCache enables the persistent cache in a private, pre-bound
// namespace directory. The caller is responsible for binding the directory to
// the account, API endpoint, and vault identity.
func (f *Fs) EnableMetadataCache(dir string) error {
	cache, err := newMetadataCache(dir)
	if err != nil {
		return err
	}
	f.metadataCache = cache
	return nil
}

// MetadataCacheStats returns cache counters without exposing local paths.
func (f *Fs) MetadataCacheStats() MetadataCacheStats {
	if f == nil || f.metadataCache == nil {
		return MetadataCacheStats{}
	}
	return f.metadataCache.Snapshot()
}

// Snapshot returns cache counters without exposing local paths.
func (c *metadataCache) Snapshot() MetadataCacheStats {
	if c == nil {
		return MetadataCacheStats{}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.stats
}

func (c *metadataCache) isDisabled() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.disabled
}

func (c *metadataCache) setDisabled(disabled bool) {
	c.mu.Lock()
	c.disabled = disabled
	c.mu.Unlock()
}

// Clear removes the bounded persistent cache and invalidates fills already in
// progress. A failed clear leaves caching disabled until restart.
func (c *metadataCache) Clear() error {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	c.generation++
	generation := c.generation
	c.disabled = true
	c.flights = make(map[string]*metadataCacheFlight)
	c.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), metadataCacheFillTimeout)
	defer cancel()
	var removed int
	err := auth.WithPrivateFileLock(ctx, c.lockPath(), func() error {
		entries, overflow, err := c.scanEntries()
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := os.Remove(filepath.Join(c.dir, entry.name)); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			removed++
		}
		if overflow || len(entries) > metadataCacheMaxEntries {
			return errors.New("metadata cache contains more files than the bounded clear can remove")
		}
		c.entries = make(map[string]metadataCacheEntry)
		c.totalBytes = 0
		c.lastSweep = c.now()
		c.updateDirectoryModTime()
		return nil
	})
	c.mu.Lock()
	c.stats.Evictions += uint64(removed)
	if err == nil && c.generation == generation {
		c.disabled = false
	}
	c.mu.Unlock()
	if err != nil {
		return fmt.Errorf("degoo: clear metadata cache: %w", err)
	}
	return nil
}

func newMetadataCache(dir string) (*metadataCache, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, errors.New("degoo: metadata cache directory is required")
	}
	absolute, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("degoo: resolve metadata cache directory: %w", err)
	}
	if err := ensureMetadataCacheDirectory(absolute); err != nil {
		return nil, fmt.Errorf("degoo: prepare metadata cache directory: %w", err)
	}
	c := &metadataCache{dir: absolute, flights: make(map[string]*metadataCacheFlight), now: time.Now, entries: make(map[string]metadataCacheEntry)}
	ctx, cancel := context.WithTimeout(context.Background(), metadataCacheFillTimeout)
	defer cancel()
	if err := auth.WithPrivateFileLock(ctx, c.lockPath(), func() error {
		if err := c.refreshLocked(ctx, true); err != nil {
			return err
		}
		if c.isDisabled() {
			return nil
		}
		evicted, err := c.evictLocked(ctx, 0, "", true)
		if evicted != 0 {
			c.mu.Lock()
			c.stats.Evictions += uint64(evicted)
			c.mu.Unlock()
		}
		return err
	}); err != nil {
		return nil, fmt.Errorf("degoo: secure metadata cache directory: %w", err)
	}
	return c, nil
}

func ensureMetadataCacheDirectory(dir string) error {
	if !filepath.IsAbs(dir) {
		return errors.New("metadata cache directory must be absolute")
	}
	var missing []string
	for current := dir; ; current = filepath.Dir(current) {
		info, err := os.Lstat(current)
		if err == nil {
			if info.Mode()&os.ModeSymlink != 0 {
				return errors.New("metadata cache path contains a symlink")
			}
			if !info.IsDir() {
				return errors.New("metadata cache path contains a non-directory")
			}
			break
		}
		if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("inspect metadata cache directory: %w", err)
		}
		missing = append(missing, current)
		parent := filepath.Dir(current)
		if parent == current {
			return fmt.Errorf("inspect metadata cache directory: %w", err)
		}
	}
	for i := len(missing) - 1; i >= 0; i-- {
		if err := os.Mkdir(missing[i], 0o700); err != nil && !errors.Is(err, os.ErrExist) {
			return fmt.Errorf("create metadata cache directory: %w", err)
		}
		info, err := os.Lstat(missing[i])
		if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return errors.New("metadata cache directory changed while creating it")
		}
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("inspect metadata cache directory: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() || info.Mode().Perm()&0o077 != 0 || info.Mode().Perm()&0o700 != 0o700 {
		return errors.New("metadata cache directory must be a real owner-private directory with mode 0700")
	}
	return nil
}

func metadataCacheKey(id string, size int64, modTime, createdTime time.Time) (string, bool) {
	if id == "" || size < 0 || (modTime.IsZero() && createdTime.IsZero()) {
		return "", false
	}
	version := struct {
		ID      string `json:"id"`
		Size    int64  `json:"size"`
		ModTime int64  `json:"mod_time_ns"`
		Created int64  `json:"created_time_ns"`
	}{ID: id, Size: size}
	if !modTime.IsZero() {
		version.ModTime = modTime.UTC().UnixNano()
	}
	if !createdTime.IsZero() {
		version.Created = createdTime.UTC().UnixNano()
	}
	encoded, err := json.Marshal(version)
	if err != nil {
		return "", false
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:]), true
}

func (c *metadataCache) lockPath() string { return filepath.Join(c.dir, ".lock") }

func (c *metadataCache) filePath(key string) string {
	return filepath.Join(c.dir, key+".cache")
}

func (c *metadataCache) read(key string, size int64) ([]byte, bool) {
	filePath := c.filePath(key)
	before, err := os.Lstat(filePath)
	if err != nil || before.Mode()&os.ModeSymlink != 0 || !before.Mode().IsRegular() || before.Mode().Perm() != 0o600 || before.Size() < 0 || before.Size() > metadataCacheMaxObjectSize+1024 {
		c.recordMiss()
		return nil, false
	}
	data, err := auth.ReadPrivateFile(filePath)
	if err != nil {
		c.recordMiss()
		return nil, false
	}
	after, err := os.Lstat(filePath)
	if err != nil || !os.SameFile(before, after) || after.Mode()&os.ModeSymlink != 0 || !after.Mode().IsRegular() || after.Mode().Perm() != 0o600 || after.Size() != before.Size() || after.Size() > metadataCacheMaxObjectSize+1024 {
		c.recordMiss()
		return nil, false
	}
	separator := bytes.IndexByte(data, '\n')
	if separator <= 0 || separator > 1024 {
		c.recordMiss()
		return nil, false
	}
	var header metadataCacheHeader
	decoder := json.NewDecoder(bytes.NewReader(data[:separator]))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&header); err != nil {
		c.recordMiss()
		return nil, false
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		c.recordMiss()
		return nil, false
	}
	content := data[separator+1:]
	now := c.now()
	if header.Version != metadataCacheVersion || header.Key != key || header.Size != size || int64(len(content)) != size || header.Stored.IsZero() || header.Stored.After(now.Add(time.Minute)) || now.Sub(header.Stored) >= metadataCacheTTL {
		c.recordMiss()
		return nil, false
	}
	sum := sha256.Sum256(content)
	if header.SHA256 != hex.EncodeToString(sum[:]) {
		c.recordMiss()
		return nil, false
	}
	c.mu.Lock()
	c.stats.Hits++
	c.mu.Unlock()
	return append([]byte(nil), content...), true
}

func (c *metadataCache) recordMiss() {
	c.mu.Lock()
	c.stats.Misses++
	c.mu.Unlock()
}

func (c *metadataCache) store(ctx context.Context, key string, content []byte) error {
	c.mu.Lock()
	generation := c.generation
	c.mu.Unlock()
	return c.storeAtGeneration(ctx, key, content, generation)
}

func (c *metadataCache) storeAtGeneration(ctx context.Context, key string, content []byte, generation uint64) error {
	if int64(len(content)) > metadataCacheMaxObjectSize {
		return errors.New("metadata cache object exceeds size limit")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return auth.WithPrivateFileLock(ctx, c.lockPath(), func() error {
		if !c.generationIsActive(generation) {
			return nil
		}
		header := metadataCacheHeader{
			Version: metadataCacheVersion,
			Key:     key,
			Size:    int64(len(content)),
			Stored:  c.now().UTC(),
		}
		sum := sha256.Sum256(content)
		header.SHA256 = hex.EncodeToString(sum[:])
		encodedHeader, err := json.Marshal(header)
		if err != nil {
			return err
		}
		encoded := make([]byte, 0, len(encodedHeader)+1+len(content))
		encoded = append(encoded, encodedHeader...)
		encoded = append(encoded, '\n')
		encoded = append(encoded, content...)
		evicted, err := c.evictLocked(ctx, int64(len(encoded)), key, false)
		if evicted != 0 {
			c.recordEvictions(evicted)
		}
		if err != nil {
			return err
		}
		if !c.generationIsActive(generation) {
			return nil
		}
		if err := auth.WritePrivateFile(c.filePath(key), encoded); err != nil {
			return err
		}
		fileInfo, err := os.Lstat(c.filePath(key))
		if err != nil || fileInfo.Mode()&os.ModeSymlink != 0 || !fileInfo.Mode().IsRegular() || fileInfo.Mode().Perm() != 0o600 {
			return errors.New("metadata cache file changed while writing")
		}
		name := filepath.Base(c.filePath(key))
		oldSize := int64(0)
		if old, ok := c.entries[name]; ok {
			oldSize = old.size
		}
		c.entries[name] = metadataCacheEntry{name: name, size: fileInfo.Size(), modTime: fileInfo.ModTime()}
		c.totalBytes += fileInfo.Size() - oldSize
		c.updateDirectoryModTime()
		c.mu.Lock()
		c.stats.Fills++
		c.mu.Unlock()
		return nil
	})
}

type metadataCacheEntry struct {
	name    string
	size    int64
	modTime time.Time
}

func (c *metadataCache) refreshLocked(ctx context.Context, force bool) error {
	now := c.now()
	dirInfo, err := os.Lstat(c.dir)
	if err != nil || dirInfo.Mode()&os.ModeSymlink != 0 || !dirInfo.IsDir() {
		return errors.New("metadata cache directory changed")
	}
	if !force && dirInfo.ModTime().Equal(c.dirModTime) && now.Sub(c.lastSweep) < metadataCacheSweepInterval && !now.Before(c.lastSweep) {
		return nil
	}
	entries, overflow, err := c.scanEntries()
	if err != nil {
		return err
	}
	if overflow {
		removed := 0
		for _, entry := range entries {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := os.Remove(filepath.Join(c.dir, entry.name)); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			removed++
		}
		c.entries = make(map[string]metadataCacheEntry)
		c.totalBytes = 0
		c.lastSweep = now
		c.updateDirectoryModTime()
		c.setDisabled(true)
		c.recordEvictions(removed)
		return nil
	}
	c.entries = make(map[string]metadataCacheEntry, len(entries))
	c.totalBytes = 0
	for _, entry := range entries {
		c.entries[entry.name] = entry
		c.totalBytes += entry.size
	}
	c.lastSweep = now
	c.updateDirectoryModTime()
	return nil
}

func (c *metadataCache) evictLocked(ctx context.Context, incomingBytes int64, replacingKey string, forceAgeSweep bool) (int, error) {
	now := c.now()
	ageSweep := forceAgeSweep || now.Sub(c.lastSweep) >= metadataCacheSweepInterval || now.Before(c.lastSweep)
	if err := c.refreshLocked(ctx, false); err != nil {
		return 0, err
	}
	if c.isDisabled() {
		return 0, nil
	}
	removed := 0
	if ageSweep {
		for name, entry := range c.entries {
			if now.Sub(entry.modTime) < metadataCacheTTL && !entry.modTime.After(now.Add(time.Minute)) {
				continue
			}
			if err := ctx.Err(); err != nil {
				return removed, err
			}
			if err := os.Remove(filepath.Join(c.dir, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
				return removed, err
			}
			delete(c.entries, name)
			c.totalBytes -= entry.size
			removed++
		}
	}
	targetName := ""
	if replacingKey != "" {
		targetName = replacingKey + ".cache"
	}
	effectiveBytes := c.totalBytes
	effectiveEntries := len(c.entries)
	if targetName != "" {
		if target, ok := c.entries[targetName]; ok {
			effectiveBytes -= target.size
			effectiveEntries--
		}
	}
	needsEviction := func() bool {
		return (incomingBytes > 0 && effectiveEntries >= metadataCacheMaxEntries) || effectiveBytes+incomingBytes > metadataCacheMaxBytes
	}
	if needsEviction() {
		ordered := make([]metadataCacheEntry, 0, len(c.entries))
		for _, entry := range c.entries {
			if entry.name != targetName {
				ordered = append(ordered, entry)
			}
		}
		sort.Slice(ordered, func(i, j int) bool { return ordered[i].modTime.Before(ordered[j].modTime) })
		for _, entry := range ordered {
			if !needsEviction() {
				break
			}
			if err := ctx.Err(); err != nil {
				return removed, err
			}
			if err := os.Remove(filepath.Join(c.dir, entry.name)); err != nil && !errors.Is(err, os.ErrNotExist) {
				return removed, err
			}
			delete(c.entries, entry.name)
			c.totalBytes -= entry.size
			effectiveBytes -= entry.size
			effectiveEntries--
			removed++
		}
	}
	if needsEviction() {
		return removed, errors.New("metadata cache cannot fit object within its bounds")
	}
	return removed, nil
}

func (c *metadataCache) scanEntries() ([]metadataCacheEntry, bool, error) {
	dir, err := os.Open(c.dir)
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = dir.Close() }()
	const maxScannedNames = metadataCacheMaxEntries + 2
	names, err := dir.Readdirnames(maxScannedNames)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, false, err
	}
	overflow := len(names) == maxScannedNames
	cacheEntries := 0
	for _, name := range names {
		if isMetadataCacheFilename(name) {
			cacheEntries++
		}
	}
	if cacheEntries > metadataCacheMaxEntries {
		overflow = true
	}
	return c.statEntries(names), overflow, nil
}

func (c *metadataCache) statEntries(names []string) []metadataCacheEntry {
	entries := make([]metadataCacheEntry, 0, len(names))
	for _, name := range names {
		if !isMetadataCacheFilename(name) {
			continue
		}
		info, err := os.Lstat(filepath.Join(c.dir, name))
		if err != nil {
			continue
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || info.Size() < 0 || info.Size() > metadataCacheMaxObjectSize+1024 {
			_ = os.Remove(filepath.Join(c.dir, name))
			continue
		}
		entries = append(entries, metadataCacheEntry{name: name, size: info.Size(), modTime: info.ModTime()})
	}
	return entries
}

func isMetadataCacheFilename(name string) bool {
	if len(name) != sha256.Size*2+len(".cache") || !strings.HasSuffix(name, ".cache") {
		return false
	}
	_, err := hex.DecodeString(strings.TrimSuffix(name, ".cache"))
	return err == nil
}

func (c *metadataCache) updateDirectoryModTime() {
	info, err := os.Lstat(c.dir)
	if err == nil && info.Mode()&os.ModeSymlink == 0 && info.IsDir() {
		c.dirModTime = info.ModTime()
	} else {
		c.dirModTime = time.Time{}
	}
}

func (c *metadataCache) recordEvictions(count int) {
	if count <= 0 {
		return
	}
	c.mu.Lock()
	c.stats.Evictions += uint64(count)
	c.mu.Unlock()
}

func (c *metadataCache) open(ctx context.Context, key string, size int64, load func(context.Context) ([]byte, error)) ([]byte, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	c.mu.Lock()
	if c.disabled {
		c.mu.Unlock()
		return load(ctx)
	}
	generation := c.generation
	c.mu.Unlock()
	if data, ok := c.read(key, size); ok {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if c.generationIsActive(generation) {
			return data, nil
		}
		return load(ctx)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	c.mu.Lock()
	if c.disabled {
		c.mu.Unlock()
		return load(ctx)
	}
	flight := c.flights[key]
	leader := flight == nil
	if leader {
		flight = &metadataCacheFlight{done: make(chan struct{})}
		c.flights[key] = flight
	}
	c.mu.Unlock()
	if leader {
		base := context.Background()
		if ctx != nil {
			base = context.WithoutCancel(ctx)
		}
		go func() {
			fillCtx, cancel := context.WithTimeout(base, metadataCacheFillTimeout)
			defer cancel()
			data, err := load(fillCtx)
			if err == nil && int64(len(data)) != size {
				err = fmt.Errorf("degoo: metadata cache fill length mismatch: got %d, want %d", len(data), size)
			}
			if err == nil && c.generationIsActive(generation) {
				_ = c.storeAtGeneration(fillCtx, key, data, generation)
			}
			c.mu.Lock()
			flight.data = data
			flight.err = err
			if c.flights[key] == flight {
				delete(c.flights, key)
			}
			close(flight.done)
			c.mu.Unlock()
		}()
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-flight.done:
		if flight.err != nil {
			return nil, flight.err
		}
		return append([]byte(nil), flight.data...), nil
	}
}

func (c *metadataCache) generationIsActive(generation uint64) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.generation == generation && !c.disabled
}

func (o *Object) openCached(ctx context.Context, options ...fs.OpenOption) (io.ReadCloser, error) {
	if err := validateRemote(o.remote); err != nil {
		return nil, err
	}
	if o.remote == "" || o.id == o.fs.opt.RootFolderID {
		return nil, fs.ErrorIsDir
	}
	if err := validateMetadataCacheOptions(options); err != nil {
		return nil, err
	}
	cache := o.fs.metadataCache
	key, versioned := metadataCacheKey(o.id, o.size, o.modTime, o.createdTime)
	if cache == nil || !versioned || o.size > metadataCacheMaxObjectSize || o.size < 0 {
		return o.openRemote(ctx, options...)
	}
	start, limit, cacheable, err := metadataCacheRange(options, o.size)
	if err != nil {
		return o.openRemote(ctx, options...)
	}
	if !cacheable {
		return o.openRemote(ctx, options...)
	}
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
	}
	data, err := cache.open(ctx, key, o.size, func(fillCtx context.Context) ([]byte, error) {
		body, err := o.openRemote(fillCtx)
		if err != nil {
			return nil, err
		}
		content, readErr := io.ReadAll(io.LimitReader(body, metadataCacheMaxObjectSize+1))
		closeErr := body.Close()
		if readErr != nil {
			return nil, readErr
		}
		if closeErr != nil {
			return nil, closeErr
		}
		if int64(len(content)) != o.size {
			return nil, fmt.Errorf("degoo: metadata cache fill length mismatch: got %d, want %d", len(content), o.size)
		}
		return content, nil
	})
	if err != nil {
		return nil, err
	}
	end := start + limit
	if end > int64(len(data)) {
		end = int64(len(data))
	}
	return &metadataCacheReadCloser{ctx: ctx, reader: bytes.NewReader(data[start:end])}, nil
}

func validateMetadataCacheOptions(options []fs.OpenOption) error {
	for _, option := range options {
		if option == nil || !option.Mandatory() {
			continue
		}
		switch option.(type) {
		case *fs.RangeOption, *fs.SeekOption:
			continue
		default:
			return fmt.Errorf("degoo: unsupported mandatory open option %q", option.String())
		}
	}
	return nil
}

func metadataCacheRange(options []fs.OpenOption, size int64) (int64, int64, bool, error) {
	openOptions := append([]fs.OpenOption(nil), options...)
	fs.FixRangeOption(openOptions, size)
	rangeCount := 0
	for _, option := range openOptions {
		switch option := option.(type) {
		case *fs.RangeOption:
			if option.Start >= 0 && option.End >= 0 && option.End < option.Start {
				return 0, 0, false, nil
			}
			rangeCount++
		case *fs.SeekOption:
			return 0, 0, false, nil
		case fs.NullOption, *fs.NullOption, *fs.HashesOption, fs.MetadataOption, *fs.MetadataOption, *fs.ChunkOption:
			// These options do not change the bytes fetched from this backend.
		default:
			if option == nil {
				return 0, 0, false, nil
			}
			key, _ := option.Header()
			if key != "" || option.Mandatory() {
				return 0, 0, false, nil
			}
		}
	}
	if rangeCount > 1 {
		return 0, 0, false, nil
	}
	expected, known, err := expectedReadLength(size, openOptions)
	if err != nil {
		return 0, 0, true, err
	}
	if !known {
		return 0, 0, false, nil
	}
	start := int64(0)
	for _, option := range openOptions {
		if rangeOption, ok := option.(*fs.RangeOption); ok {
			start, _ = rangeOption.Decode(size)
			break
		}
	}
	return start, expected, true, nil
}

type metadataCacheReadCloser struct {
	ctx    context.Context
	reader *bytes.Reader
}

func (r *metadataCacheReadCloser) Read(p []byte) (int, error) {
	if r.ctx != nil {
		if err := r.ctx.Err(); err != nil {
			return 0, err
		}
	}
	return r.reader.Read(p)
}

func (r *metadataCacheReadCloser) Close() error { return nil }
