package vaultnamespace

import (
	"errors"
	"fmt"
	"math/rand"
	"testing"
	"time"

	"github.com/rclone/rclone/backend/degoo/internal/vaultmanifest"
)

type modelSnapshot struct {
	snapshot Snapshot
	entries  map[string]Entry
}

const modelNameCount = 12

func TestNamespaceRandomRootOperationsMatchModel(t *testing.T) {
	f := newNamespaceFixture(t)
	current := f.init(t)
	model := make(map[string]Entry)
	var history []modelSnapshot
	rng := rand.New(rand.NewSource(52))
	var nextID uint64 = 1
	var day = 2
	names := make([]string, modelNameCount)
	for i := range names {
		names[i] = fmt.Sprintf("entry-%02d", i)
	}

	for step := 0; step < 120; step++ {
		name := names[rng.Intn(len(names))]
		oldRoot := f.lookup(t, current, "")
		before := cloneModelEntries(model)
		now := namespaceTime(day)
		switch rng.Intn(5) {
		case 0:
			updated, err := f.ns.Mkdir(f.ctx, current, name, now)
			if existing, ok := model[name]; ok {
				if existing.Kind == Directory {
					if err != nil || updated != current {
						t.Fatalf("step %d mkdir existing directory: snapshot/error=%v/%v", step, updated, err)
					}
				} else if !errors.Is(err, ErrExists) {
					t.Fatalf("step %d mkdir over file error=%v", step, err)
				}
			} else {
				if err != nil {
					t.Fatalf("step %d mkdir %q: %v", step, name, err)
				}
				current = updated
				model[name] = Entry{ID: f.lookup(t, current, name).ID, ParentID: current.RootID, Name: name, Kind: Directory, Revision: 1, ModTime: now.UTC()}
				assertRootChanged(t, f, current, oldRoot, now)
			}
		case 1:
			id := modelID(nextID)
			nextID++
			content := f.manifestFor(t, id, fmt.Sprintf("data-%d", nextID))
			updated, err := f.ns.CreateFile(f.ctx, current, name, content, now)
			if _, ok := model[name]; ok {
				if !errors.Is(err, ErrExists) {
					t.Fatalf("step %d create over existing entry error=%v", step, err)
				}
			} else {
				if err != nil {
					t.Fatalf("step %d create %q: %v", step, name, err)
				}
				current = updated
				model[name] = Entry{ID: id, ParentID: current.RootID, Name: name, Kind: File, Revision: 1, Size: content.Size, ModTime: now.UTC(), Content: &content}
				assertRootChanged(t, f, current, oldRoot, now)
			}
		case 2:
			old, ok := model[name]
			if !ok || old.Kind != File {
				_, err := f.ns.ReplaceFile(f.ctx, current, name, old, vaultmanifest.Manifest{}, now)
				if ok && old.Kind == Directory {
					if !errors.Is(err, ErrIsDirectory) {
						t.Fatalf("step %d replace directory error=%v", step, err)
					}
				} else if !errors.Is(err, ErrNotFound) {
					t.Fatalf("step %d replace missing file error=%v", step, err)
				}
				break
			}
			content := f.manifestFor(t, old.ID, fmt.Sprintf("replacement-%d", step))
			updated, err := f.ns.ReplaceFile(f.ctx, current, name, old, content, now)
			if err != nil {
				t.Fatalf("step %d replace %q: %v", step, name, err)
			}
			current = updated
			old.Revision++
			old.Size = content.Size
			old.ModTime = now.UTC()
			old.Content = &content
			model[name] = old
		case 3:
			old, ok := model[name]
			if !ok {
				if _, err := f.ns.Remove(f.ctx, current, name, Entry{}, now); !errors.Is(err, ErrNotFound) {
					t.Fatalf("step %d remove missing %q error=%v", step, name, err)
				}
				break
			}
			updated, err := f.ns.Remove(f.ctx, current, name, old, now)
			if err != nil {
				t.Fatalf("step %d remove %q: %v", step, name, err)
			}
			current = updated
			delete(model, name)
			assertRootChanged(t, f, current, oldRoot, now)
		default:
			destination := names[rng.Intn(len(names))]
			old, ok := model[name]
			if !ok {
				if _, err := f.ns.Rename(f.ctx, current, name, destination, Entry{}, now); !errors.Is(err, ErrNotFound) {
					t.Fatalf("step %d rename missing %q error=%v", step, name, err)
				}
				break
			}
			if destination == name {
				updated, err := f.ns.Rename(f.ctx, current, name, destination, old, now)
				if err != nil || updated != current {
					t.Fatalf("step %d self rename snapshot/error=%v/%v", step, updated, err)
				}
				break
			}
			target, occupied := model[destination]
			if occupied && target.Kind != old.Kind {
				_, err := f.ns.Rename(f.ctx, current, name, destination, old, now)
				if old.Kind == Directory && target.Kind == File {
					if !errors.Is(err, ErrNotDirectory) {
						t.Fatalf("step %d rename directory over file error=%v", step, err)
					}
				} else if !errors.Is(err, ErrIsDirectory) {
					t.Fatalf("step %d rename file over directory error=%v", step, err)
				}
				break
			}
			updated, err := f.ns.Rename(f.ctx, current, name, destination, old, now)
			if err != nil {
				t.Fatalf("step %d rename %q to %q: %v", step, name, destination, err)
			}
			current = updated
			delete(model, name)
			if occupied {
				delete(model, destination)
			}
			old.Revision++
			old.Name = destination
			old.ModTime = before[name].ModTime
			model[destination] = old
			assertRootChanged(t, f, current, oldRoot, now)
		}

		if step%10 == 0 {
			history = append(history, modelSnapshot{snapshot: current, entries: cloneModelEntries(model)})
		}
		assertModel(t, f, current, model)
		day++
	}

	for _, snapshot := range history {
		assertModel(t, f, snapshot.snapshot, snapshot.entries)
	}
}

func modelID(value uint64) (id [16]byte) {
	id[0] = 0xa5
	for i := 0; i < 8; i++ {
		id[15-i] = byte(value >> (8 * i))
	}
	return id
}

func cloneModelEntries(entries map[string]Entry) map[string]Entry {
	cloned := make(map[string]Entry, len(entries))
	for name, entry := range entries {
		if entry.Content != nil {
			content := *entry.Content
			entry.Content = &content
		}
		cloned[name] = entry
	}
	return cloned
}

func assertModel(t *testing.T, f namespaceFixture, snapshot Snapshot, want map[string]Entry) {
	t.Helper()
	var listed = make(map[string]Entry)
	if err := f.ns.List(f.ctx, snapshot, "", func(entry Entry) error {
		listed[entry.Name] = entry
		return nil
	}); err != nil {
		t.Fatalf("list model snapshot: %v", err)
	}
	if len(listed) != len(want) {
		t.Fatalf("listed %d entries, model has %d", len(listed), len(want))
	}
	for name, expected := range want {
		got, ok := listed[name]
		if !ok {
			t.Fatalf("listing omitted %q", name)
		}
		assertModelEntry(t, name, got, expected)
		lookedUp, err := f.ns.Lookup(f.ctx, snapshot, name)
		if err != nil {
			t.Fatalf("lookup %q: %v", name, err)
		}
		assertModelEntry(t, name, lookedUp, expected)
	}
	for i := 0; i < modelNameCount; i++ {
		name := fmt.Sprintf("entry-%02d", i)
		if _, exists := want[name]; exists {
			continue
		}
		if _, err := f.ns.Lookup(f.ctx, snapshot, name); !errors.Is(err, ErrNotFound) {
			t.Fatalf("lookup removed entry %q error=%v", name, err)
		}
	}
}

func assertModelEntry(t *testing.T, name string, got, want Entry) {
	t.Helper()
	if got.ID != want.ID || got.ParentID != want.ParentID || got.Name != want.Name || got.Kind != want.Kind || got.Revision != want.Revision || got.Size != want.Size || !got.ModTime.Equal(want.ModTime) {
		t.Fatalf("entry %q = %#v, want %#v", name, got, want)
	}
	if (got.Content == nil) != (want.Content == nil) || (got.Content != nil && *got.Content != *want.Content) {
		t.Fatalf("entry %q content = %#v, want %#v", name, got.Content, want.Content)
	}
}

func assertRootChanged(t *testing.T, f namespaceFixture, current Snapshot, old Entry, now time.Time) {
	t.Helper()
	got, err := f.ns.Lookup(f.ctx, current, "")
	if err != nil {
		t.Fatalf("lookup root after mutation: %v", err)
	}
	wantRevision := old.Revision + 1
	if got.Revision != wantRevision || !got.ModTime.Equal(now.UTC()) {
		t.Fatalf("root after mutation = revision %d time %v, want revision %d time %v", got.Revision, got.ModTime, wantRevision, now.UTC())
	}
}
