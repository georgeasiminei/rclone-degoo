package degoo

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"
)

// These limits bound this experiment only. The snapshot copier below is
// separate from the radix prototype and is not a selected remote format.
// Allocation and cleanup assume a single actor; only root writes model fences.
// Inventories cover metadata pages only; data references are symbolic.
const (
	publicationMaxRecords   = 64
	publicationMaxStaged    = 8
	publicationMaxStageByte = 512 * 1024
	publicationOrphanLimit  = 8
	publicationInventoryCap = 16
	publicationFanout       = 16
	publicationRouteDepth   = 16
)

var (
	errPublicationBefore   = errors.New("injected root write failure before apply")
	errPublicationLost     = errors.New("injected root write response loss after apply")
	errPublicationConflict = errors.New("root revision changed")
	errPublicationFence    = errors.New("writer epoch is stale")
	errPublicationOrphans  = errors.New("orphan backlog blocks publication")
	errPublicationBudget   = errors.New("staging or inventory budget exhausted")
	errPublicationUnknown  = errors.New("publication or inventory is incomplete")
)

type publicationCaps struct {
	ConditionalRootWrite bool
	EnforcedWriterFence  bool
	StrongRootRead       bool
}

type publicationHead struct {
	RootID   string
	Revision uint64
	Epoch    uint64
}

type publicationFault uint8

const (
	publicationNoFault publicationFault = iota
	publicationBeforeWrite
	publicationAfterWrite
	publicationLostBeforeWrite
)

type publicationCounters struct {
	RootWrites     int `json:"root_writes"`
	HeadReads      int `json:"head_reads"`
	CleanupScanned int `json:"cleanup_scanned"`
	CleanupDeleted int `json:"cleanup_deleted"`
}

type publicationObjectPath struct {
	role  string
	route string
}

// publicationPlacement counts files and child directories in each physical
// directory, including the vault root and control directories.
type publicationPlacement struct {
	dirs     map[string]map[string]struct{}
	paths    map[string]publicationObjectPath
	limit    int
	maxDepth int
	maxSeen  int
}

func newPublicationPlacement(limit, maxDepth int) *publicationPlacement {
	return &publicationPlacement{
		dirs:     map[string]map[string]struct{}{"/vault": {}},
		paths:    make(map[string]publicationObjectPath),
		limit:    limit,
		maxDepth: maxDepth,
	}
}

func (p *publicationPlacement) add(role, route, id string) error {
	if role == "" || route == "" || id == "" || p.limit <= 0 || p.maxDepth <= 0 {
		return errors.New("invalid physical allocation")
	}
	if _, exists := p.paths[id]; exists {
		return errors.New("duplicate physical object ID")
	}
	parts := []string{role}
	for index := 0; index < min(len(route), p.maxDepth); index++ {
		parts = append(parts, route[index:index+1])
	}
	parent := "/vault"
	newDirs := make([]struct{ parent, edge, path string }, 0, len(parts))
	counts := make(map[string]int, len(parts)+1)
	for _, part := range parts {
		if _, ok := counts[parent]; !ok {
			counts[parent] = len(p.dirs[parent])
		}
		edge := "d:" + part
		child := parent + "/" + part
		_, exists := p.dirs[parent][edge]
		if !exists {
			if counts[parent]+1 > p.limit {
				return errors.New("physical directory fanout exhausted")
			}
			counts[parent]++
			newDirs = append(newDirs, struct{ parent, edge, path string }{parent, edge, child})
		}
		parent = child
	}
	if _, ok := counts[parent]; !ok {
		counts[parent] = len(p.dirs[parent])
	}
	if counts[parent]+1 > p.limit {
		return errors.New("physical allocator exhausted its route depth")
	}
	for _, dir := range newDirs {
		p.dirs[dir.parent][dir.edge] = struct{}{}
		p.dirs[dir.path] = make(map[string]struct{})
	}
	p.dirs[parent]["f:"+id] = struct{}{}
	p.paths[id] = publicationObjectPath{role: role, route: route}
	for _, dir := range p.dirs {
		p.maxSeen = max(p.maxSeen, len(dir))
	}
	return nil
}

func (p *publicationPlacement) remove(id string) {
	object, ok := p.paths[id]
	if !ok {
		return
	}
	parts := []string{object.role}
	for index := 0; index < min(len(object.route), p.maxDepth); index++ {
		parts = append(parts, object.route[index:index+1])
	}
	paths := []string{"/vault"}
	for _, part := range parts {
		paths = append(paths, paths[len(paths)-1]+"/"+part)
	}
	leaf := p.dirs[paths[len(paths)-1]]
	delete(leaf, "f:"+id)
	for index := len(parts); index > 0; index-- {
		childPath := paths[index]
		if len(p.dirs[childPath]) != 0 {
			break
		}
		delete(p.dirs, childPath)
		delete(p.dirs[paths[index-1]], "d:"+parts[index-1])
	}
	delete(p.paths, id)
}

func (p *publicationPlacement) maximum() int {
	maximum := 0
	for _, entries := range p.dirs {
		maximum = max(maximum, len(entries))
	}
	return maximum
}

type publicationRecord struct {
	ParentID string
	Name     string
	Kind     string
	StableID string
	DataID   string
}

type publicationCandidate struct {
	RootID    string
	Inventory []string
}

type publicationModel struct {
	index                *shardModel
	placement            *publicationPlacement
	objectPaths          map[string]publicationObjectPath
	stagedBytesByID      map[string]int
	stagedBytes          int
	stagedObjects        int
	capabilities         publicationCaps
	providerHead         publicationHead
	visibleHead          publicationHead
	authorityEpoch       uint64
	current              *publicationCandidate
	previous             *publicationCandidate
	uncertainPublication bool
	inventoryComplete    bool
	orphanBudget         int
	orphans              map[string]struct{}
	counters             publicationCounters
}

func newPublicationModel(t *testing.T, caps publicationCaps) *publicationModel {
	t.Helper()
	model := &publicationModel{
		index:             newShardModel(t),
		placement:         newPublicationPlacement(publicationFanout, publicationRouteDepth),
		objectPaths:       make(map[string]publicationObjectPath),
		stagedBytesByID:   make(map[string]int),
		capabilities:      caps,
		inventoryComplete: true,
		authorityEpoch:    1,
		orphanBudget:      publicationOrphanLimit,
		orphans:           make(map[string]struct{}),
	}
	if err := model.placement.add("control", strings.Repeat("0", publicationRouteDepth), "HEAD"); err != nil {
		t.Fatal(err)
	}
	return model
}

func publicationClone(candidate *publicationCandidate) *publicationCandidate {
	if candidate == nil {
		return nil
	}
	return &publicationCandidate{RootID: candidate.RootID, Inventory: append([]string(nil), candidate.Inventory...)}
}

func publicationContains(ids []string, id string) bool {
	for _, candidate := range ids {
		if candidate == id {
			return true
		}
	}
	return false
}

func publicationRecordsForIndex(model *publicationModel, records map[string]publicationRecord) []shardModelRecord {
	keys := make([]string, 0, len(records))
	for key := range records {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	indexRecords := make([]shardModelRecord, 0, len(keys))
	for _, key := range keys {
		record := records[key]
		name := record.ParentID + "\x00" + record.Name
		id := record.Kind + "\x00" + record.StableID + "\x00" + record.DataID
		indexRecords = append(indexRecords, shardModelRecord{Key: model.index.logicalKey(name), Name: name, ID: id})
	}
	return indexRecords
}

// stageSnapshot preflights the record, encrypted-node, staging-object, and
// staging-byte bounds before writing. Its inventory is collected after each
// modeled write, so it does not establish durable intent-before-network.
func (m *publicationModel) stageSnapshot(records map[string]publicationRecord) (publicationCandidate, error) {
	if !m.inventoryComplete || m.uncertainPublication {
		return publicationCandidate{}, errPublicationUnknown
	}
	if len(records) > publicationMaxRecords {
		return publicationCandidate{}, errors.New("snapshot record bound exceeded")
	}
	indexRecords := publicationRecordsForIndex(m, records)
	nodePlain, err := json.Marshal(shardModelNode{Records: indexRecords})
	if err != nil {
		return publicationCandidate{}, err
	}
	if len(nodePlain)+m.index.aead.NonceSize()+m.index.aead.Overhead() > shardModelMaxBytes {
		return publicationCandidate{}, errors.New("encrypted index node byte bound exceeded")
	}
	indexID := fmt.Sprintf("model-id-%08d", m.index.nextID+1)
	rootPlain, err := json.Marshal(shardModelNode{Children: map[string]string{"index": indexID}})
	if err != nil {
		return publicationCandidate{}, err
	}
	rootID := fmt.Sprintf("model-id-%08d", m.index.nextID+2)
	if len(rootPlain)+m.index.aead.NonceSize()+m.index.aead.Overhead() > shardModelMaxBytes {
		return publicationCandidate{}, errors.New("encrypted root node byte bound exceeded")
	}
	estimatedBytes := len(nodePlain) + len(rootPlain) + 2*(m.index.aead.NonceSize()+m.index.aead.Overhead())
	if m.stagedObjects+2 > publicationMaxStaged || m.stagedBytes+estimatedBytes > publicationMaxStageByte {
		return publicationCandidate{}, errPublicationBudget
	}
	before := make(map[string]struct{}, len(m.index.objects))
	for id := range m.index.objects {
		before[id] = struct{}{}
	}
	indexRoot, err := m.index.build(indexRecords, 0)
	if err == nil {
		rootID, err = m.index.store(shardModelNode{Children: map[string]string{"index": indexRoot}})
	}
	inventory, registerErr := m.registerNew(before, rootID)
	if err != nil {
		return publicationCandidate{RootID: rootID, Inventory: inventory}, err
	}
	if registerErr != nil {
		return publicationCandidate{RootID: rootID, Inventory: inventory}, registerErr
	}
	if len(inventory) != 2 || inventory[0] == inventory[1] {
		return publicationCandidate{RootID: rootID, Inventory: inventory}, errors.New("snapshot inventory did not match its bounded index and root writes")
	}
	return publicationCandidate{RootID: rootID, Inventory: inventory}, nil
}

func (m *publicationModel) registerNew(before map[string]struct{}, rootID string) ([]string, error) {
	ids := make([]string, 0, 2)
	for id, ciphertext := range m.index.objects {
		if _, existed := before[id]; existed {
			continue
		}
		role := "index"
		if id == rootID {
			role = "roots"
		}
		sum := sha256.Sum256([]byte(role + "\x00" + id))
		route := hex.EncodeToString(sum[:])
		if err := m.placement.add(role, route, id); err != nil {
			m.inventoryComplete = false
			return ids, err
		}
		m.objectPaths[id] = publicationObjectPath{role: role, route: route}
		m.stagedBytesByID[id] = len(ciphertext)
		m.stagedBytes += len(ciphertext)
		m.stagedObjects++
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids, nil
}

func (m *publicationModel) loadSnapshot(rootID string) (map[string]publicationRecord, error) {
	index := make(map[string]string)
	if err := m.index.rebuild(rootID, index); err != nil {
		return nil, err
	}
	records := make(map[string]publicationRecord, len(index))
	for recordName, recordID := range index {
		name := strings.SplitN(recordName, "\x00", 2)
		value := strings.SplitN(recordID, "\x00", 3)
		if len(name) != 2 || len(value) != 3 {
			return nil, errors.New("invalid generated index record")
		}
		key := publicationPathKey(name[0], name[1])
		records[key] = publicationRecord{ParentID: name[0], Name: name[1], Kind: value[0], StableID: value[1], DataID: value[2]}
	}
	return records, nil
}

func (m *publicationModel) installInitial(candidate publicationCandidate) {
	m.providerHead = publicationHead{RootID: candidate.RootID, Revision: 1, Epoch: m.authorityEpoch}
	m.visibleHead = m.providerHead
	m.current = publicationClone(&candidate)
	m.releaseStaging(candidate.Inventory)
}

func (m *publicationModel) takeoverWriterEpoch() uint64 {
	m.authorityEpoch++ // A modeled authority issues the epoch; writers cannot choose it.
	return m.authorityEpoch
}

func (m *publicationModel) publishRoot(candidate publicationCandidate, expectedRevision, writerEpoch uint64, fault publicationFault) error {
	if !m.inventoryComplete || m.uncertainPublication {
		return errPublicationUnknown
	}
	if m.expectedOrphans(candidate) >= m.orphanBudget {
		return errPublicationOrphans
	}
	if m.capabilities.ConditionalRootWrite && expectedRevision != m.providerHead.Revision {
		return errPublicationConflict
	}
	if m.capabilities.EnforcedWriterFence && writerEpoch != m.authorityEpoch {
		return errPublicationFence
	}
	m.counters.RootWrites++
	if fault == publicationBeforeWrite {
		return errPublicationBefore
	}
	if fault == publicationLostBeforeWrite {
		m.uncertainPublication = true
		return errPublicationLost
	}
	dropped := publicationClone(m.previous)
	m.previous = publicationClone(m.current)
	m.current = publicationClone(&candidate)
	m.providerHead = publicationHead{RootID: candidate.RootID, Revision: m.providerHead.Revision + 1, Epoch: writerEpoch}
	if m.capabilities.StrongRootRead {
		m.visibleHead = m.providerHead
	}
	m.refreshOrphans(dropped)
	m.releaseStaging(candidate.Inventory)
	if fault == publicationAfterWrite {
		m.uncertainPublication = true
		return errPublicationLost
	}
	return nil
}

func (m *publicationModel) expectedOrphans(candidate publicationCandidate) int {
	ids := make(map[string]struct{}, len(m.orphans)+2)
	for id := range m.orphans {
		ids[id] = struct{}{}
	}
	if m.previous != nil {
		for _, id := range m.previous.Inventory {
			if m.current == nil || !publicationContains(m.current.Inventory, id) {
				if !publicationContains(candidate.Inventory, id) {
					ids[id] = struct{}{}
				}
			}
		}
	}
	for _, id := range candidate.Inventory {
		delete(ids, id)
	}
	return len(ids)
}

func (m *publicationModel) refreshOrphans(dropped *publicationCandidate) {
	for _, pinned := range []*publicationCandidate{m.current, m.previous} {
		if pinned != nil {
			for _, id := range pinned.Inventory {
				delete(m.orphans, id)
			}
		}
	}
	if dropped != nil {
		for _, id := range dropped.Inventory {
			if !m.isPinned(id) {
				m.orphans[id] = struct{}{}
			}
		}
	}
}

func (m *publicationModel) isPinned(id string) bool {
	return (m.current != nil && publicationContains(m.current.Inventory, id)) ||
		(m.previous != nil && publicationContains(m.previous.Inventory, id))
}

func (m *publicationModel) releaseStaging(ids []string) {
	for _, id := range ids {
		if size, ok := m.stagedBytesByID[id]; ok {
			m.stagedBytes -= size
			m.stagedObjects--
			delete(m.stagedBytesByID, id)
		}
	}
}

func (m *publicationModel) recordOrphans(ids []string) error {
	newIDs := 0
	for _, id := range ids {
		if !m.isPinned(id) {
			if _, exists := m.orphans[id]; !exists {
				newIDs++
			}
		}
	}
	if len(m.orphans)+newIDs > publicationInventoryCap {
		return errPublicationBudget
	}
	for _, id := range ids {
		if !m.isPinned(id) {
			m.orphans[id] = struct{}{}
		}
	}
	m.releaseStaging(ids)
	return nil
}

func (m *publicationModel) readRoot() publicationHead {
	m.counters.HeadReads++
	if m.capabilities.StrongRootRead {
		return m.providerHead
	}
	return m.visibleHead
}

// cleanupStep inspects only a bounded in-memory inventory. It refuses to act
// when root publication or the transaction inventory is uncertain.
func (m *publicationModel) cleanupStep(maxWork int) (int, error) {
	if maxWork < 2 {
		return 0, errors.New("cleanup needs at least one inspection and one deletion slot")
	}
	if !m.inventoryComplete || m.uncertainPublication {
		return 0, errPublicationUnknown
	}
	ids := make([]string, 0, len(m.orphans))
	for id := range m.orphans {
		ids = append(ids, id)
	}
	sort.Strings(ids) // The complete queue is capped by publicationInventoryCap.
	work, deleted := 0, 0
	for _, id := range ids {
		if work >= maxWork {
			break
		}
		work++
		m.counters.CleanupScanned++
		if m.isPinned(id) {
			delete(m.orphans, id)
			continue
		}
		if work >= maxWork {
			break
		}
		if _, exists := m.objectPaths[id]; !exists {
			m.inventoryComplete = false
			return deleted, errPublicationUnknown
		}
		delete(m.index.objects, id)
		m.placement.remove(id)
		delete(m.objectPaths, id)
		deleted++
		m.counters.CleanupDeleted++
		delete(m.orphans, id)
		work++
	}
	return deleted, nil
}

func publicationPathKey(parentID, name string) string { return parentID + "\x00" + name }

func publicationEntry(parent, name, kind, stable, data string) publicationRecord {
	return publicationRecord{ParentID: parent, Name: name, Kind: kind, StableID: stable, DataID: data}
}

func TestScalabilityPublicationRootFaultsAndBounds(t *testing.T) {
	makeInitial := func(t *testing.T) (*publicationModel, publicationCandidate) {
		t.Helper()
		m := newPublicationModel(t, publicationCaps{})
		candidate, err := m.stageSnapshot(map[string]publicationRecord{
			publicationPathKey("root-dir", "old"): publicationEntry("root-dir", "old", "file", "stable-file", "data-id"),
		})
		if err != nil {
			t.Fatal(err)
		}
		m.installInitial(candidate)
		return m, candidate
	}
	newRecords := map[string]publicationRecord{
		publicationPathKey("root-dir", "new"): publicationEntry("root-dir", "new", "file", "stable-file", "data-id"),
	}
	failed, old := makeInitial(t)
	candidate, err := failed.stageSnapshot(newRecords)
	if err != nil {
		t.Fatal(err)
	}
	before := failed.providerHead
	if err := failed.publishRoot(candidate, before.Revision, 1, publicationBeforeWrite); !errors.Is(err, errPublicationBefore) {
		t.Fatalf("pre-write failure: %v", err)
	}
	if failed.providerHead != before {
		t.Fatal("pre-write failure changed root")
	}
	if records, err := failed.loadSnapshot(old.RootID); err != nil || records[publicationPathKey("root-dir", "old")].DataID != "data-id" {
		t.Fatalf("old complete root unreadable: %v", err)
	}
	if err := failed.recordOrphans(candidate.Inventory); err != nil {
		t.Fatal(err)
	}
	if _, err := failed.cleanupStep(3); err != nil {
		t.Fatalf("known pre-write orphan cleanup: %v", err)
	}

	// An applied write with a lost response and an unapplied write can return
	// the same error and stale read. That observation cannot prove absence.
	ambiguous, _ := makeInitial(t)
	ambiguousCandidate, err := ambiguous.stageSnapshot(newRecords)
	if err != nil {
		t.Fatal(err)
	}
	initialHead := ambiguous.providerHead
	if err := ambiguous.publishRoot(ambiguousCandidate, initialHead.Revision, 1, publicationAfterWrite); !errors.Is(err, errPublicationLost) {
		t.Fatalf("ambiguous root result: %v", err)
	}
	clientRead := ambiguous.readRoot()
	if clientRead != initialHead || ambiguous.providerHead.RootID != ambiguousCandidate.RootID {
		t.Fatalf("stale-read ambiguity missing: client=%+v provider=%+v", clientRead, ambiguous.providerHead)
	}
	absent, _ := makeInitial(t)
	absentCandidate, err := absent.stageSnapshot(newRecords)
	if err != nil {
		t.Fatal(err)
	}
	if err := absent.publishRoot(absentCandidate, absent.providerHead.Revision, 1, publicationLostBeforeWrite); !errors.Is(err, errPublicationLost) {
		t.Fatalf("unapplied ambiguous root result: %v", err)
	}
	if absent.readRoot() != clientRead || absent.providerHead == ambiguous.providerHead {
		t.Fatal("the stale-reading client distinguished absent and applied writes")
	}
	if _, err := ambiguous.cleanupStep(3); !errors.Is(err, errPublicationUnknown) {
		t.Fatalf("uncertain root was eligible for cleanup: %v", err)
	}
	if _, err := absent.cleanupStep(3); !errors.Is(err, errPublicationUnknown) {
		t.Fatalf("unapplied but unconfirmed write was eligible for cleanup: %v", err)
	}
	for _, model := range []*publicationModel{ambiguous, absent} {
		writes := model.index.counts.Writes
		if _, err := model.stageSnapshot(newRecords); !errors.Is(err, errPublicationUnknown) || model.index.counts.Writes != writes {
			t.Fatal("unresolved publication admitted more staging")
		}
	}

	tooMany := make(map[string]publicationRecord, publicationMaxRecords+1)
	for index := 0; index <= publicationMaxRecords; index++ {
		name := fmt.Sprintf("generated-%03d", index)
		tooMany[publicationPathKey("root", name)] = publicationEntry("root", name, "file", name, "data")
	}
	beforeWrites := failed.index.counts.Writes
	if _, err := failed.stageSnapshot(tooMany); err == nil || failed.index.counts.Writes != beforeWrites {
		t.Fatal("oversized snapshot was not rejected before writes")
	}
	oversized := map[string]publicationRecord{
		publicationPathKey("root", "oversized"): publicationEntry("root", strings.Repeat("x", shardModelMaxBytes), "file", "id", "data"),
	}
	if _, err := failed.stageSnapshot(oversized); err == nil || failed.index.counts.Writes != beforeWrites {
		t.Fatal("oversized encrypted node was not rejected before writes")
	}
	budgeted := newPublicationModel(t, publicationCaps{})
	for index := 0; index < publicationMaxStaged/2; index++ {
		if _, err := budgeted.stageSnapshot(newRecords); err != nil {
			t.Fatal(err)
		}
	}
	budgetWrites := budgeted.index.counts.Writes
	if _, err := budgeted.stageSnapshot(newRecords); !errors.Is(err, errPublicationBudget) || budgeted.index.counts.Writes != budgetWrites || budgeted.stagedObjects != publicationMaxStaged {
		t.Fatalf("staging admission was not bounded before writes: err=%v writes=%d staged=%d", err, budgeted.index.counts.Writes, budgeted.stagedObjects)
	}
	unknown := newPublicationModel(t, publicationCaps{})
	unknown.inventoryComplete = false
	if _, err := unknown.stageSnapshot(newRecords); !errors.Is(err, errPublicationUnknown) || unknown.index.counts.Writes != 0 {
		t.Fatal("incomplete inventory admitted more staging")
	}
	t.Logf("root faults: failed=%+v ambiguous=%+v; index=%+v", failed.counters, ambiguous.counters, failed.index.counts)
}

func TestScalabilityPublicationFenceAndStableRename(t *testing.T) {
	makeWriters := func(t *testing.T, caps publicationCaps) (*publicationModel, publicationCandidate, publicationCandidate) {
		t.Helper()
		m := newPublicationModel(t, caps)
		base, err := m.stageSnapshot(map[string]publicationRecord{
			publicationPathKey("parent-id", "before"): publicationEntry("parent-id", "before", "directory", "stable-dir", ""),
			publicationPathKey("stable-dir", "file"):  publicationEntry("stable-dir", "file", "file", "stable-file", "stable-data"),
		})
		if err != nil {
			t.Fatal(err)
		}
		m.installInitial(base)
		a, err := m.stageSnapshot(map[string]publicationRecord{
			publicationPathKey("parent-id", "a"): publicationEntry("parent-id", "a", "file", "writer-a", "data-a"),
		})
		if err != nil {
			t.Fatal(err)
		}
		b, err := m.stageSnapshot(map[string]publicationRecord{
			publicationPathKey("parent-id", "b"): publicationEntry("parent-id", "b", "file", "writer-b", "data-b"),
		})
		if err != nil {
			t.Fatal(err)
		}
		return m, a, b
	}

	unfenced, writerA, writerB := makeWriters(t, publicationCaps{StrongRootRead: true})
	if err := unfenced.publishRoot(writerA, 1, 1, publicationNoFault); err != nil {
		t.Fatal(err)
	}
	if got := unfenced.readRoot(); got.RootID != writerA.RootID {
		t.Fatalf("strong readback missed published root: %+v", got)
	}
	epochB := unfenced.takeoverWriterEpoch()
	if err := unfenced.publishRoot(writerB, 2, epochB, publicationNoFault); err != nil {
		t.Fatal(err)
	}
	if err := unfenced.publishRoot(writerA, 3, 1, publicationNoFault); err != nil {
		t.Fatal(err)
	}
	if got := unfenced.readRoot(); got.RootID != writerA.RootID {
		t.Fatalf("unfenced stale writer did not replace B: %+v", got)
	}

	casOnly, casA, casB := makeWriters(t, publicationCaps{ConditionalRootWrite: true, StrongRootRead: true})
	casEpoch := casOnly.takeoverWriterEpoch()
	if err := casOnly.publishRoot(casB, 1, casEpoch, publicationNoFault); err != nil {
		t.Fatal(err)
	}
	if err := casOnly.publishRoot(casA, 1, 1, publicationNoFault); !errors.Is(err, errPublicationConflict) {
		t.Fatalf("CAS alone accepted an old revision: %v", err)
	}
	if err := casOnly.publishRoot(casA, 2, 1, publicationNoFault); err != nil || casOnly.providerHead.RootID != casA.RootID {
		t.Fatalf("CAS alone unexpectedly fenced a stale owner using the current revision: %v", err)
	}

	fenced, staleA, writerB := makeWriters(t, publicationCaps{ConditionalRootWrite: true, EnforcedWriterFence: true, StrongRootRead: true})
	epochB = fenced.takeoverWriterEpoch()
	if err := fenced.publishRoot(writerB, 1, epochB, publicationNoFault); err != nil {
		t.Fatal(err)
	}
	if err := fenced.publishRoot(staleA, 1, epochB, publicationNoFault); !errors.Is(err, errPublicationConflict) {
		t.Fatalf("expected revision did not reject a conflicting root: %v", err)
	}
	if err := fenced.publishRoot(staleA, 2, epochB+1, publicationNoFault); !errors.Is(err, errPublicationFence) {
		t.Fatalf("writer self-granted a future epoch: %v", err)
	}
	if err := fenced.publishRoot(staleA, 2, 1, publicationNoFault); !errors.Is(err, errPublicationFence) {
		t.Fatalf("provider-enforced epoch did not reject stale writer: %v", err)
	}
	if got := fenced.readRoot(); got.RootID != writerB.RootID {
		t.Fatalf("stale writer replaced fenced root: %+v", got)
	}

	rename := newPublicationModel(t, publicationCaps{})
	old, err := rename.stageSnapshot(map[string]publicationRecord{
		publicationPathKey("parent-id", "before"): publicationEntry("parent-id", "before", "directory", "stable-dir", ""),
		publicationPathKey("stable-dir", "file"):  publicationEntry("stable-dir", "file", "file", "stable-file", "stable-data"),
	})
	if err != nil {
		t.Fatal(err)
	}
	moved, err := rename.stageSnapshot(map[string]publicationRecord{
		publicationPathKey("parent-id", "after"): publicationEntry("parent-id", "after", "directory", "stable-dir", ""),
		publicationPathKey("stable-dir", "file"): publicationEntry("stable-dir", "file", "file", "stable-file", "stable-data"),
	})
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := rename.loadSnapshot(moved.RootID)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := loaded[publicationPathKey("parent-id", "before")]; ok {
		t.Fatal("rename retained source name")
	}
	if got := loaded[publicationPathKey("parent-id", "after")]; got.StableID != "stable-dir" {
		t.Fatalf("rename changed directory ID: %+v", got)
	}
	if got := loaded[publicationPathKey("stable-dir", "file")]; got.StableID != "stable-file" || got.DataID != "stable-data" {
		t.Fatalf("rename changed descendant data ID: %+v", got)
	}
	for _, id := range old.Inventory {
		if bytes.Contains(rename.index.objects[id], []byte("stable-dir")) || bytes.Contains(rename.index.objects[id], []byte("before")) {
			t.Fatal("generated encrypted index exposed plaintext metadata")
		}
	}
	t.Logf("writer counters: unfenced=%+v fenced=%+v rename=%+v", unfenced.counters, fenced.counters, rename.index.counts)
}

func TestScalabilityPublicationPlacementAndOrphanCleanup(t *testing.T) {
	placement := newPublicationPlacement(publicationFanout, publicationRouteDepth)
	roles := []string{"data", "index", "roots", "control", "txn", "staging"}
	for _, role := range roles {
		for index := 0; index < 96; index++ {
			id := fmt.Sprintf("generated-%s-%03d", role, index)
			sum := sha256.Sum256([]byte(role + "\x00" + id))
			if err := placement.add(role, hex.EncodeToString(sum[:]), id); err != nil {
				t.Fatalf("allocate %s: %v", id, err)
			}
		}
	}
	for path, entries := range placement.dirs {
		if len(entries) > publicationFanout {
			t.Fatalf("directory %s has %d files/children, limit %d", path, len(entries), publicationFanout)
		}
	}
	if placement.dirs["/vault"] == nil || placement.dirs["/vault/control"] == nil || placement.maximum() > publicationFanout {
		t.Fatal("vault root/control placement was omitted or exceeded its bound")
	}
	short := newPublicationPlacement(4, 1)
	for index := 0; index < 4; index++ {
		if err := short.add("control", "a", fmt.Sprintf("entry-%d", index)); err != nil {
			t.Fatal(err)
		}
	}
	if err := short.add("control", "a", "overflow"); err == nil {
		t.Fatal("allocator exhaustion did not fail closed")
	}

	model := newPublicationModel(t, publicationCaps{StrongRootRead: true})
	base, err := model.stageSnapshot(map[string]publicationRecord{
		publicationPathKey("root", "base"): publicationEntry("root", "base", "file", "base", "base-data"),
	})
	if err != nil {
		t.Fatal(err)
	}
	model.installInitial(base)
	previous, err := model.stageSnapshot(map[string]publicationRecord{
		publicationPathKey("root", "previous"): publicationEntry("root", "previous", "file", "previous", "previous-data"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := model.publishRoot(previous, 1, 1, publicationNoFault); err != nil {
		t.Fatal(err)
	}
	for index := 0; index < publicationOrphanLimit-1; index++ {
		before := make(map[string]struct{}, len(model.index.objects))
		for id := range model.index.objects {
			before[id] = struct{}{}
		}
		id, err := model.index.store(shardModelNode{Records: []shardModelRecord{{Key: "orphan", Name: fmt.Sprintf("orphan-%d", index), ID: "generated"}}})
		if err != nil {
			t.Fatal(err)
		}
		ids, err := model.registerNew(before, "")
		if err != nil || len(ids) != 1 || ids[0] != id {
			t.Fatalf("orphan inventory registration: ids=%v err=%v", ids, err)
		}
		if err := model.recordOrphans(ids); err != nil {
			t.Fatal(err)
		}
	}
	blocked, err := model.stageSnapshot(map[string]publicationRecord{
		publicationPathKey("root", "later"): publicationEntry("root", "later", "file", "later", "data"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := model.publishRoot(blocked, 2, 1, publicationNoFault); !errors.Is(err, errPublicationOrphans) {
		t.Fatalf("orphan budget did not block publication: %v", err)
	}
	if !model.isPinned(base.RootID) || !model.isPinned(previous.RootID) {
		t.Fatal("current and previous complete roots were not pinned")
	}

	steps := 0
	for len(model.orphans) > 0 {
		beforeScan, beforeDelete := model.counters.CleanupScanned, model.counters.CleanupDeleted
		deleted, err := model.cleanupStep(3)
		if err != nil {
			t.Fatal(err)
		}
		work := model.counters.CleanupScanned - beforeScan + model.counters.CleanupDeleted - beforeDelete
		if work > 3 || deleted > 1 || !model.isPinned(base.RootID) || !model.isPinned(previous.RootID) {
			t.Fatalf("cleanup exceeded work/pin bound: work=%d deleted=%d", work, deleted)
		}
		steps++
		if steps > publicationInventoryCap*2 {
			t.Fatal("bounded cleanup did not make progress")
		}
	}
	if len(model.orphans) > publicationInventoryCap {
		t.Fatal("orphan inventory exceeded its hard bound")
	}
	model.inventoryComplete = false
	if deleted, err := model.cleanupStep(3); !errors.Is(err, errPublicationUnknown) || deleted != 0 {
		t.Fatalf("incomplete inventory did not fail closed: deleted=%d err=%v", deleted, err)
	}
	unknown := newPublicationModel(t, publicationCaps{})
	unknown.orphans["generated-unknown-path"] = struct{}{}
	if deleted, err := unknown.cleanupStep(3); !errors.Is(err, errPublicationUnknown) || deleted != 0 {
		t.Fatalf("missing object identity did not block cleanup: deleted=%d err=%v", deleted, err)
	}
	if _, retained := unknown.orphans["generated-unknown-path"]; !retained || unknown.inventoryComplete {
		t.Fatal("cleanup discarded an unreconciled object reservation")
	}
	t.Logf("placement dirs=%d max=%d; cleanup steps=%d counters=%+v", len(placement.dirs), placement.maximum(), steps, model.counters)
}
