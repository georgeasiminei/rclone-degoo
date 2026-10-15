//go:build linux

package degoo

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// This fixture compares append-only physical placement strategies. Its
// whole-map JSON checkpoint tests local reopen only; it has no bounded page
// admission or provider-side discovery, fencing, or delete completion.
var (
	errAdaptivePlacementFull     = errors.New("adaptive placement capacity exhausted")
	errAdaptivePlacementUnknown  = errors.New("adaptive placement outcome is unresolved")
	errAdaptivePlacementPending  = errors.New("adaptive placement has a pending allocation")
	errAdaptivePlacementInjected = errors.New("injected adaptive placement interruption")
	errAdaptiveCleanupBudget     = errors.New("adaptive cleanup work budget exhausted")
)

type adaptivePlacementFault uint8

const (
	adaptivePlacementNoFault adaptivePlacementFault = iota
	adaptivePlacementBeforeFolder
	adaptivePlacementAfterFolder
)

type adaptivePlacementResolution uint8

const (
	adaptivePlacementApplyPrepared adaptivePlacementResolution = iota + 1
	adaptivePlacementPresent
	adaptivePlacementAbsent
	adaptivePlacementStillUnknown
)

type adaptivePlacementPending struct {
	ID, Role, Route, Status, Parent, Edge, Path string
}

type adaptivePlacementDir struct {
	Depth, Reserve        int
	Children, PendingDirs map[string]string
	Objects, Ghosts       map[string]bool
}

type adaptivePlacementCounters struct {
	FolderCreates, ObjectAllocations, CleanupInspected, CleanupDeleted, CleanupDirs int
}

type adaptivePlacement struct {
	Fanout, Reserve, ObjectLimit, MaxDepth int
	Dirs                                   map[string]*adaptivePlacementDir
	Paths                                  map[string]string
	Pending                                *adaptivePlacementPending
	Uncertain                              bool
	Counters                               adaptivePlacementCounters
}

type adaptivePlacementStats struct {
	Objects, Dirs, ChildDirs, Reserved, Unresolved, Ghosts, MaxEntries, MaxDepth int
}

func newAdaptivePlacement(fanout, reserve, objectLimit, maxDepth int, roles []string) *adaptivePlacement {
	p := &adaptivePlacement{
		Fanout: fanout, Reserve: reserve, ObjectLimit: objectLimit, MaxDepth: maxDepth,
		Dirs: make(map[string]*adaptivePlacementDir), Paths: make(map[string]string),
	}
	p.Dirs["/vault"] = newAdaptivePlacementDir(0, reserve)
	for _, role := range roles {
		path := "/vault/" + role
		p.Dirs["/vault"].Children["d:"+role] = path
		p.Dirs[path] = newAdaptivePlacementDir(0, reserve)
	}
	return p
}

func newAdaptivePlacementDir(depth, reserve int) *adaptivePlacementDir {
	return &adaptivePlacementDir{
		Depth:       depth,
		Reserve:     reserve,
		Children:    make(map[string]string),
		Objects:     make(map[string]bool),
		PendingDirs: make(map[string]string),
		Ghosts:      make(map[string]bool),
	}
}

func adaptiveRouteBits(sum [sha256.Size]byte) string {
	route := make([]byte, 0, len(sum)*8)
	for _, b := range sum {
		for bit := 7; bit >= 0; bit-- {
			route = append(route, '0'+(b>>uint(bit)&1))
		}
	}
	return string(route)
}

func (d *adaptivePlacementDir) entries() int {
	return len(d.Children) + len(d.Objects) + len(d.PendingDirs) + len(d.Ghosts)
}

func (p *adaptivePlacement) add(role, id, route string) error {
	return p.addWithFault(role, id, route, adaptivePlacementNoFault)
}

func (p *adaptivePlacement) addWithFault(role, id, route string, fault adaptivePlacementFault) error {
	if p.Uncertain {
		return errAdaptivePlacementUnknown
	}
	if p.Pending != nil {
		return errAdaptivePlacementPending
	}
	if role == "" || id == "" || route == "" || p.Fanout <= 0 || p.MaxDepth < 0 {
		return errors.New("invalid adaptive placement request")
	}
	if _, ok := p.Paths[id]; ok {
		return errors.New("duplicate adaptive object ID")
	}
	path := "/vault/" + role
	if p.Dirs[path] == nil {
		return errors.New("unknown adaptive placement role")
	}
	depth := 0
	for {
		d := p.Dirs[path]
		if len(d.Objects) < p.ObjectLimit {
			if d.entries()+d.Reserve+1 > p.Fanout {
				return errAdaptivePlacementFull
			}
			d.Objects[id], p.Paths[id] = true, path
			p.Counters.ObjectAllocations++
			return nil
		}
		if depth >= p.MaxDepth || depth >= len(route) {
			return errAdaptivePlacementFull
		}
		edge := "d:" + string(route[depth])
		if ghost := d.Ghosts[edge]; ghost {
			return errAdaptivePlacementUnknown
		}
		if child, ok := d.Children[edge]; ok {
			path = child
			depth++
			continue
		}
		child := path + "/" + edge[2:]
		if d.entries()+d.Reserve+1 > p.Fanout {
			return errAdaptivePlacementFull
		}
		d.PendingDirs[edge] = child
		p.Pending = &adaptivePlacementPending{ID: id, Role: role, Route: route, Status: "prepared", Parent: path, Edge: edge, Path: child}
		switch fault {
		case adaptivePlacementBeforeFolder:
			return errAdaptivePlacementInjected
		case adaptivePlacementAfterFolder:
			p.Pending.Status = "unknown"
			p.Uncertain = true
			return errAdaptivePlacementInjected
		}
		p.applyAdaptivePending()
		path = child
		depth++
	}
}

func (p *adaptivePlacement) applyAdaptivePending() {
	pending := p.Pending
	parent := p.Dirs[pending.Parent]
	delete(parent.PendingDirs, pending.Edge)
	parent.Children[pending.Edge] = pending.Path
	p.Dirs[pending.Path] = newAdaptivePlacementDir(parent.Depth+1, p.Reserve)
	p.Counters.FolderCreates++
	p.Pending = nil
	p.Uncertain = false
}

func (p *adaptivePlacement) resolvePending(resolution adaptivePlacementResolution) error {
	if p.Pending == nil {
		return errors.New("no adaptive allocation is pending")
	}
	pending := p.Pending
	if pending.Status == "prepared" {
		if resolution != adaptivePlacementApplyPrepared {
			return errors.New("prepared placement requires the modeled operation to run")
		}
	} else if resolution == adaptivePlacementStillUnknown {
		p.Uncertain = true
		return errAdaptivePlacementUnknown
	} else if resolution == adaptivePlacementAbsent {
		delete(p.Dirs[pending.Parent].PendingDirs, pending.Edge)
		p.Pending, p.Uncertain = nil, false
		return p.add(pending.Role, pending.ID, pending.Route)
	} else if resolution != adaptivePlacementPresent {
		return errors.New("invalid adaptive placement resolution")
	}
	p.applyAdaptivePending()
	return p.add(pending.Role, pending.ID, pending.Route)
}

func (p *adaptivePlacement) markGhost(path, edge string) error {
	d := p.Dirs[path]
	if d == nil || edge == "" {
		return errors.New("unknown adaptive ghost location")
	}
	if d.Ghosts[edge] || d.Children[edge] != "" || d.PendingDirs[edge] != "" {
		return errAdaptivePlacementUnknown
	}
	if d.entries()+d.Reserve+1 > p.Fanout {
		return errAdaptivePlacementFull
	}
	d.Ghosts[edge] = true
	return nil
}

func (p *adaptivePlacement) stats() adaptivePlacementStats {
	stats := adaptivePlacementStats{Dirs: len(p.Dirs), Objects: len(p.Paths)}
	for _, d := range p.Dirs {
		stats.ChildDirs += len(d.Children)
		stats.Reserved += d.Reserve
		stats.Unresolved += len(d.PendingDirs)
		stats.Ghosts += len(d.Ghosts)
		stats.MaxEntries = max(stats.MaxEntries, d.entries()+d.Reserve)
		stats.MaxDepth = max(stats.MaxDepth, d.Depth)
	}
	return stats
}

func (p *adaptivePlacement) validate() error {
	if p.Fanout <= 0 || p.Reserve < 0 || p.ObjectLimit <= 0 || p.Dirs["/vault"] == nil {
		return errors.New("invalid adaptive placement state")
	}
	seen := make(map[string]bool, len(p.Dirs))
	pendingSlots := 0
	var walk func(string) error
	walk = func(path string) error {
		if seen[path] {
			return errors.New("physical directory graph contains a cycle or duplicate child")
		}
		d := p.Dirs[path]
		if d == nil {
			return errors.New("physical directory child is missing")
		}
		seen[path] = true
		if d.entries()+d.Reserve > p.Fanout || d.Reserve != p.Reserve || d.Depth > p.MaxDepth || len(d.Objects) > p.ObjectLimit {
			return errors.New("physical directory exceeds placement bounds")
		}
		pendingSlots += len(d.PendingDirs)
		for id := range d.Objects {
			if !d.Objects[id] || p.Paths[id] != path {
				return errors.New("physical object has no matching locator")
			}
		}
		for edge := range d.Ghosts {
			if d.Children[edge] != "" || d.PendingDirs[edge] != "" {
				return errors.New("ghost folder slot is reused")
			}
		}
		for edge, child := range d.Children {
			childDir := p.Dirs[child]
			if edge == "" || childDir == nil || childDir.Depth != d.Depth+1 && path != "/vault" || path == "/vault" && childDir.Depth != 0 {
				return errors.New("physical directory has an invalid child")
			}
			if err := walk(child); err != nil {
				return err
			}
		}
		return nil
	}
	if err := walk("/vault"); err != nil {
		return err
	}
	if len(seen) != len(p.Dirs) {
		return errors.New("physical directory state has unreachable paths")
	}
	for id, path := range p.Paths {
		if p.Dirs[path] == nil || !p.Dirs[path].Objects[id] {
			return errors.New("object locator has no matching physical allocation")
		}
	}
	if p.Pending != nil {
		parent := p.Dirs[p.Pending.Parent]
		if parent == nil || parent.PendingDirs[p.Pending.Edge] != p.Pending.Path || pendingSlots != 1 ||
			(p.Pending.Status != "prepared" && p.Pending.Status != "unknown") || p.Uncertain != (p.Pending.Status == "unknown") {
			return errors.New("pending folder reservation is inconsistent")
		}
	} else if pendingSlots != 0 || p.Uncertain {
		return errors.New("placement state has an unowned unresolved reservation")
	}
	return nil
}

func (p *adaptivePlacement) save(path string) error {
	if err := p.validate(); err != nil {
		return err
	}
	data, err := json.Marshal(p)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	f, err := os.OpenFile(tmp, os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	err = f.Sync()
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	err = dir.Sync()
	closeErr = dir.Close()
	if err != nil {
		return err
	}
	return closeErr
}

func openAdaptivePlacement(path string) (*adaptivePlacement, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var p adaptivePlacement
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, err
	}
	if err := p.validate(); err != nil {
		return nil, err
	}
	return &p, nil
}

// cleanupObject removes one known unreferenced object and any newly empty
// physical directories. Inspection, object deletion, and directory removal
// each consume one cleanup work unit.
func (p *adaptivePlacement) cleanupObject(id string, protected map[string]bool, budget int) (int, error) {
	if p.Uncertain || p.Pending != nil {
		return 0, errAdaptivePlacementUnknown
	}
	if err := p.validate(); err != nil {
		return 0, err
	}
	if budget < 1 {
		return 0, errAdaptiveCleanupBudget
	}
	if protected[id] {
		p.Counters.CleanupInspected++
		return 1, nil
	}
	path, ok := p.Paths[id]
	if !ok {
		p.Counters.CleanupInspected++
		return 1, errors.New("cleanup object has no known physical identity")
	}
	prune := p.prunableDirsAfterRemoving(path, id)
	needed := 2 + len(prune)
	if needed > budget {
		return 0, errAdaptiveCleanupBudget
	}
	p.Counters.CleanupInspected++
	d := p.Dirs[path]
	delete(d.Objects, id)
	delete(p.Paths, id)
	p.Counters.CleanupDeleted++
	work := 2
	for _, emptyPath := range prune {
		parentPath := filepath.Dir(emptyPath)
		parent := p.Dirs[parentPath]
		if parent == nil {
			return work, errors.New("cleanup parent directory disappeared")
		}
		for edge, child := range parent.Children {
			if child == emptyPath {
				delete(parent.Children, edge)
				break
			}
		}
		delete(p.Dirs, emptyPath)
		p.Counters.CleanupDirs++
		work++
	}
	return work, p.validate()
}

func (p *adaptivePlacement) prunableDirsAfterRemoving(path, id string) []string {
	var prune []string
	current := path
	removedChild := ""
	for current != "/vault" && current != "" {
		d := p.Dirs[current]
		if d == nil {
			break
		}
		objects := len(d.Objects)
		if current == path {
			objects--
		}
		children := len(d.Children)
		if removedChild != "" {
			for _, child := range d.Children {
				if child == removedChild {
					children--
					break
				}
			}
		}
		if objects != 0 || children != 0 || len(d.PendingDirs) != 0 || len(d.Ghosts) != 0 {
			break
		}
		if filepath.Dir(current) == "/vault" {
			break
		}
		prune = append(prune, current)
		removedChild = current
		current = filepath.Dir(current)
	}
	return prune
}

func adaptiveReopen(t *testing.T, p *adaptivePlacement, path string) *adaptivePlacement {
	t.Helper()
	if err := p.save(path); err != nil {
		t.Fatal(err)
	}
	reopened, err := openAdaptivePlacement(path)
	if err != nil {
		t.Fatal(err)
	}
	return reopened
}

func adaptiveFillDirect(t *testing.T, p *adaptivePlacement) {
	t.Helper()
	for index := 0; index < p.ObjectLimit; index++ {
		id := fmt.Sprintf("direct-%02d", index)
		if err := p.add("data", id, strings.Repeat("0", 32)); err != nil {
			t.Fatal(err)
		}
	}
}

func TestScalabilityAdaptivePlacementComparison(t *testing.T) {
	roles := []string{"data", "index", "roots", "control", "txn", "staging"}
	adaptive := newAdaptivePlacement(16, 2, 12, 16, roles)
	fixed := newPublicationPlacement(publicationFanout, publicationRouteDepth)
	for _, role := range roles {
		for index := 0; index < 96; index++ {
			id := fmt.Sprintf("generated-%s-%03d", role, index)
			sum := sha256.Sum256([]byte(role + "\x00" + id))
			if err := fixed.add(role, hex.EncodeToString(sum[:]), id); err != nil {
				t.Fatalf("fixed-depth allocate %s: %v", id, err)
			}
			if err := adaptive.add(role, id, adaptiveRouteBits(sum)); err != nil {
				t.Fatalf("adaptive allocate %s: %v", id, err)
			}
		}
	}
	if err := adaptive.validate(); err != nil {
		t.Fatal(err)
	}
	for path, entries := range fixed.dirs {
		if len(entries) > publicationFanout {
			t.Fatalf("fixed directory %s exceeds occupancy: %d", path, len(entries))
		}
	}
	stats := adaptive.stats()
	if stats.Objects != 576 || stats.MaxEntries > adaptive.Fanout || stats.MaxDepth > adaptive.MaxDepth || stats.Reserved != stats.Dirs*adaptive.Reserve || stats.Unresolved != 0 || stats.Ghosts != 0 {
		t.Fatalf("adaptive occupancy violated bound: %+v", stats)
	}
	if len(fixed.dirs) != 8650 {
		t.Fatalf("fixed-depth comparator changed: dirs=%d, want historical 8650", len(fixed.dirs))
	}
	if stats.Dirs >= len(fixed.dirs)/10 {
		t.Fatalf("adaptive placement did not reduce directory overhead enough: adaptive=%d fixed=%d", stats.Dirs, len(fixed.dirs))
	}
	statePath := filepath.Join(t.TempDir(), "placement.json")
	if err := adaptive.save(statePath); err != nil {
		t.Fatal(err)
	}
	checkpointInfo, err := os.Stat(statePath)
	if err != nil {
		t.Fatal(err)
	}
	reopened, err := openAdaptivePlacement(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if got := reopened.stats(); got != stats {
		t.Fatalf("reopened placement changed occupancy: got=%+v want=%+v", got, stats)
	}
	for id, path := range adaptive.Paths {
		if reopened.Paths[id] != path {
			t.Fatalf("reopen moved append-only object %s: got=%s want=%s", id, reopened.Paths[id], path)
		}
	}
	used := stats.Objects + stats.ChildDirs + stats.Reserved + stats.Unresolved + stats.Ghosts
	t.Logf("576 generated objects: adaptive dirs=%d entries=%d slots=%d reserved=%d unresolved=%d ghosts=%d max=%d depth=%d checkpoint=%dB; fixed-depth dirs=%d max=%d", stats.Dirs, stats.Objects+stats.ChildDirs, used, stats.Reserved, stats.Unresolved, stats.Ghosts, stats.MaxEntries, stats.MaxDepth, checkpointInfo.Size(), len(fixed.dirs), fixed.maximum())
}

func TestScalabilityAdaptivePlacementVersionsSkewAndExhaustion(t *testing.T) {
	roles := []string{"data"}
	versions := newAdaptivePlacement(16, 2, 12, 12, roles)
	firstPaths := make(map[string]string)
	for version := 0; version < 48; version++ {
		id := fmt.Sprintf("logical-file-version-%03d", version)
		sum := sha256.Sum256([]byte("data\x00" + id))
		if err := versions.add("data", id, adaptiveRouteBits(sum)); err != nil {
			t.Fatalf("allocate repeated version %s: %v", id, err)
		}
		if version == 11 {
			for oldID, oldPath := range versions.Paths {
				firstPaths[oldID] = oldPath
			}
		}
	}
	if versions.stats().Objects != 48 {
		t.Fatal("repeated versions were overwritten or omitted")
	}
	for id, path := range firstPaths {
		if versions.Paths[id] != path {
			t.Fatalf("later version moved %s from %s to %s", id, path, versions.Paths[id])
		}
	}

	skewed := newAdaptivePlacement(6, 1, 3, 2, roles)
	for index := 0; index < 9; index++ {
		id := fmt.Sprintf("skew-version-%d", index)
		if err := skewed.add("data", id, strings.Repeat("0", 16)); err != nil {
			t.Fatalf("allocate skewed version %d: %v", index, err)
		}
	}
	before := skewed.stats()
	if err := skewed.add("data", "skew-overflow", strings.Repeat("0", 16)); !errors.Is(err, errAdaptivePlacementFull) {
		t.Fatalf("skew beyond maximum depth did not fail closed: %v", err)
	}
	if after := skewed.stats(); after != before {
		t.Fatalf("exhausted allocation changed state: before=%+v after=%+v", before, after)
	}
	if err := skewed.markGhost("/vault/data", "d:1"); err != nil {
		t.Fatal(err)
	}
	ghostStats := skewed.stats()
	if err := skewed.markGhost("/vault/data", "d:0"); err == nil || skewed.stats() != ghostStats || skewed.validate() != nil {
		t.Fatal("ghost marker collided with an existing child directory")
	}
	if err := skewed.markGhost("/vault/data", "d:1"); err == nil || skewed.stats() != ghostStats {
		t.Fatal("duplicate ghost marker changed occupancy")
	}
	if ghostStats.Ghosts != 1 || ghostStats.MaxEntries > skewed.Fanout {
		t.Fatalf("ghost folder was not counted against physical capacity: %+v", ghostStats)
	}
	t.Logf("48 repeated versions: dirs=%d max=%d; skew: objects=%d dirs=%d max=%d depth=%d; exhausted without mutation", versions.stats().Dirs, versions.stats().MaxEntries, before.Objects, before.Dirs, before.MaxEntries, before.MaxDepth)
}

func TestScalabilityAdaptivePlacementInterruptionsAndCleanup(t *testing.T) {
	roles := []string{"data"}
	statePath := filepath.Join(t.TempDir(), "placement.json")

	beforeFolder := newAdaptivePlacement(16, 2, 12, 8, roles)
	adaptiveFillDirect(t, beforeFolder)
	if err := beforeFolder.addWithFault("data", "before-folder", strings.Repeat("0", 32), adaptivePlacementBeforeFolder); !errors.Is(err, errAdaptivePlacementInjected) {
		t.Fatalf("before-folder interruption missing: %v", err)
	}
	pendingStats := beforeFolder.stats()
	if err := beforeFolder.markGhost("/vault/data", "d:0"); err == nil || beforeFolder.stats() != pendingStats || beforeFolder.validate() != nil {
		t.Fatal("ghost marker collided with an unresolved directory reservation")
	}
	reopened := adaptiveReopen(t, beforeFolder, statePath)
	if reopened.stats().Unresolved != 1 || !errors.Is(reopened.add("data", "racer", strings.Repeat("1", 32)), errAdaptivePlacementPending) {
		t.Fatal("prepared split did not retain its reserved directory slot across reopen")
	}
	if err := reopened.resolvePending(adaptivePlacementApplyPrepared); err != nil {
		t.Fatalf("resume prepared folder create: %v", err)
	}
	if _, ok := reopened.Paths["before-folder"]; !ok {
		t.Fatal("resumed split did not finish its object allocation")
	}

	afterFolder := newAdaptivePlacement(16, 2, 12, 8, roles)
	adaptiveFillDirect(t, afterFolder)
	if err := afterFolder.addWithFault("data", "after-folder", strings.Repeat("0", 32), adaptivePlacementAfterFolder); !errors.Is(err, errAdaptivePlacementInjected) {
		t.Fatalf("lost folder acknowledgement not injected: %v", err)
	}
	reopened = adaptiveReopen(t, afterFolder, statePath)
	if err := reopened.resolvePending(adaptivePlacementStillUnknown); !errors.Is(err, errAdaptivePlacementUnknown) {
		t.Fatalf("unknown folder outcome did not stop allocation: %v", err)
	}
	if !errors.Is(reopened.add("data", "blocked", strings.Repeat("1", 32)), errAdaptivePlacementUnknown) {
		t.Fatal("uncertain folder creation admitted more allocations")
	}
	if err := reopened.resolvePending(adaptivePlacementPresent); err != nil {
		t.Fatalf("reconcile visible folder and resume allocation: %v", err)
	}
	if _, ok := reopened.Paths["after-folder"]; !ok || reopened.stats().Unresolved != 0 {
		t.Fatal("visible split was not resumed exactly once")
	}

	cleanup := newAdaptivePlacement(4, 1, 2, 4, roles)
	for _, id := range []string{"current-root", "previous-root", "orphan-a", "orphan-b"} {
		if err := cleanup.add("data", id, strings.Repeat("0", 16)); err != nil {
			t.Fatal(err)
		}
	}
	protected := map[string]bool{"current-root": true, "previous-root": true}
	workA, err := cleanup.cleanupObject("orphan-a", protected, 3)
	if err != nil {
		t.Fatal(err)
	}
	workB, err := cleanup.cleanupObject("orphan-b", protected, 3)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := cleanup.Paths["current-root"]; !ok {
		t.Fatal("cleanup removed the current root")
	}
	if _, ok := cleanup.Paths["previous-root"]; !ok {
		t.Fatal("cleanup removed the previous root")
	}
	if cleanup.Counters.CleanupDeleted != 2 || cleanup.Counters.CleanupDirs != 1 || workA > 3 || workB > 3 {
		t.Fatalf("cleanup exceeded work budget or missed empty-folder cleanup: work=%d/%d counters=%+v", workA, workB, cleanup.Counters)
	}
	if err := cleanup.markGhost("/vault/data", "d:1"); err != nil {
		t.Fatal(err)
	}
	if work, err := cleanup.cleanupObject("missing-id", nil, 3); err == nil || work > 3 {
		t.Fatalf("cleanup treated unknown identity as safe: work=%d err=%v", work, err)
	}
	deep := newAdaptivePlacement(4, 1, 1, 4, roles)
	for _, id := range []string{"level-0", "level-1", "level-2", "leaf"} {
		if err := deep.add("data", id, strings.Repeat("0", 16)); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []string{"level-0", "level-1", "level-2"} {
		if work, err := deep.cleanupObject(id, nil, 2); err != nil || work != 2 {
			t.Fatalf("remove emptying ancestor object: work=%d err=%v", work, err)
		}
	}
	deepBefore, countersBefore := deep.stats(), deep.Counters
	if work, err := deep.cleanupObject("leaf", nil, 4); !errors.Is(err, errAdaptiveCleanupBudget) || work != 0 || deep.stats() != deepBefore || deep.Counters != countersBefore {
		t.Fatalf("insufficient cleanup budget mutated deep tree: work=%d err=%v", work, err)
	}
	work, err := deep.cleanupObject("leaf", nil, 5)
	if err != nil || work != 5 || deep.Counters.CleanupDirs != 3 || deep.stats().Dirs != 2 {
		t.Fatalf("exact cleanup budget did not prune the full empty chain: work=%d dirs=%d err=%v", work, deep.Counters.CleanupDirs, err)
	}
	t.Logf("placement interruption recovery: folders=%d unresolved=%d; cleanup work=%d/%d counters=%+v; deep work=%d counters=%+v", reopened.Counters.FolderCreates, reopened.stats().Unresolved, workA, workB, cleanup.Counters, work, deep.Counters)
}
