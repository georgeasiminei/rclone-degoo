//go:build linux

package degoo

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// This is a test-only, single-process model. Its local journal is durable
// safety state, while remote slot discovery and writer fencing are simulated
// assumptions. GraphComplete is only a local completeness bit; it cannot prove
// authenticated or fresh remote state. Reopening means constructing a new store over the same files;
// it does not simulate host power loss, remote paged-intent bootstrap, cold
// cache loss, cross-machine recovery, or prove Degoo supports these operations.
// A physical-slot intent covers any modeled remote create, including a folder
// or object. The test does not choose a production journal or remote schema.
const (
	durableJournalFile       = "allocation-journal.json"
	durablePendingFile       = "pending.data"
	durableJournalVersion    = 1
	durableJournalMaxBytes   = 16 * 1024
	durableSlotLimit         = 8
	durableCleanupStepLimit  = 2
	durableIDMaxBytes        = 64
	durableJournalEntryLimit = 2 * durableSlotLimit
)

var (
	errDurableJournalMissing  = errors.New("durable allocation journal is missing")
	errDurableJournalCorrupt  = errors.New("durable allocation journal is corrupt")
	errDurableJournalTooBig   = errors.New("durable allocation journal exceeds its byte bound")
	errDurableSlotBudget      = errors.New("physical allocation slot budget exhausted")
	errDurableIntentUnknown   = errors.New("allocation outcome is unresolved")
	errDurableAckLost         = errors.New("simulated remote acknowledgement was lost")
	errDurableRemoteAbsent    = errors.New("authoritative remote discovery found no created object")
	errDurableRemotePresent   = errors.New("authoritative remote discovery still finds the object")
	errDurableFenced          = errors.New("simulated writer epoch is stale")
	errDurablePoisoned        = errors.New("journal checkpoint failed; reopen before more work")
	errDurableGraphIncomplete = errors.New("cleanup requires a complete local reference graph")
)

type durableObject struct {
	ID         string   `json:"id"`
	Slot       uint64   `json:"slot"`
	Role       string   `json:"role"`
	References []string `json:"references,omitempty"`
}

type durableIntent struct {
	Operation   string `json:"operation"`
	Transaction string `json:"transaction"`
	ObjectID    string `json:"object_id,omitempty"`
	Slot        uint64 `json:"slot"`
	Role        string `json:"role,omitempty"`
}

type durableJournal struct {
	Version       int             `json:"version"`
	Epoch         uint64          `json:"epoch"`
	SlotLimit     uint64          `json:"slot_limit"`
	NextSlot      uint64          `json:"next_slot"`
	PendingFile   string          `json:"pending_file"`
	CurrentRoot   string          `json:"current_root,omitempty"`
	PreviousRoot  string          `json:"previous_root,omitempty"`
	GraphComplete bool            `json:"graph_complete"`
	ReaderPins    []string        `json:"reader_pins,omitempty"`
	Objects       []durableObject `json:"objects,omitempty"`
	Intents       []durableIntent `json:"intents,omitempty"`
	CleanupIDs    []string        `json:"cleanup_ids,omitempty"`
	CleanupIndex  uint64          `json:"cleanup_index"`
}

type simulatedRemoteObject struct {
	ID          string
	Transaction string
	Role        string
	Slot        uint64
}

type simulatedRemote struct {
	objects       map[uint64]simulatedRemoteObject
	createCalls   int
	deleteCalls   int
	discoverCalls int
	beforeCreate  func(slot uint64, transaction string) error
	beforeDelete  func(slot uint64, id string) error
}

func newSimulatedRemote() *simulatedRemote {
	return &simulatedRemote{objects: make(map[uint64]simulatedRemoteObject)}
}

type simulatedCreateOutcome uint8

const (
	createAckApplied simulatedCreateOutcome = iota
	createAckLostAfterApply
	createAckLostBeforeApply
)

func (r *simulatedRemote) create(slot uint64, transaction, role string, outcome simulatedCreateOutcome) (simulatedRemoteObject, error) {
	if r.beforeCreate != nil {
		if err := r.beforeCreate(slot, transaction); err != nil {
			return simulatedRemoteObject{}, fmt.Errorf("intent was not durable before remote create: %w", err)
		}
	}
	r.createCalls++
	if outcome != createAckLostBeforeApply {
		if _, exists := r.objects[slot]; exists {
			return simulatedRemoteObject{}, errors.New("simulated physical slot was reused")
		}
		remoteObject := simulatedRemoteObject{
			ID:          fmt.Sprintf("remote-%d", slot),
			Transaction: transaction,
			Role:        role,
			Slot:        slot,
		}
		r.objects[slot] = remoteObject
		if outcome == createAckApplied {
			return remoteObject, nil
		}
		return simulatedRemoteObject{}, errDurableAckLost
	}
	return simulatedRemoteObject{}, errDurableAckLost
}

type simulatedDeleteOutcome uint8

const (
	deleteAckApplied simulatedDeleteOutcome = iota
	deleteAckLostAfterApply
	deleteAckLostBeforeApply
)

func (r *simulatedRemote) delete(slot uint64, id string, outcome simulatedDeleteOutcome) error {
	if r.beforeDelete != nil {
		if err := r.beforeDelete(slot, id); err != nil {
			return fmt.Errorf("delete intent was not durable before remote delete: %w", err)
		}
	}
	r.deleteCalls++
	remoteObject, exists := r.objects[slot]
	if !exists || remoteObject.ID != id {
		return errors.New("simulated delete target did not match its physical slot")
	}
	if outcome != deleteAckLostBeforeApply {
		delete(r.objects, slot)
		if outcome == deleteAckLostAfterApply {
			return errDurableAckLost
		}
	}
	if outcome == deleteAckLostBeforeApply {
		return errDurableAckLost
	}
	return nil
}

type remoteVisibility uint8

const (
	remoteVisibilityUnknown remoteVisibility = iota
	remoteVisibilityAuthoritative
)

func (r *simulatedRemote) discover(slot uint64, visibility remoteVisibility) (simulatedRemoteObject, bool, error) {
	r.discoverCalls++
	if visibility == remoteVisibilityUnknown {
		return simulatedRemoteObject{}, false, errDurableIntentUnknown
	}
	object, exists := r.objects[slot]
	return object, exists, nil
}

// simulatedEpochAuthority stands in for an external authority that both issues
// epochs and atomically rejects a side effect from a stale epoch.
type simulatedEpochAuthority struct {
	mu    sync.Mutex
	epoch uint64
}

func newSimulatedEpochAuthority(epoch uint64) *simulatedEpochAuthority {
	return &simulatedEpochAuthority{epoch: epoch}
}

func (a *simulatedEpochAuthority) require(epoch uint64) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if epoch != a.epoch {
		return errDurableFenced
	}
	return nil
}

func (a *simulatedEpochAuthority) run(epoch uint64, action func() error) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if epoch != a.epoch {
		return errDurableFenced
	}
	return action()
}

func (a *simulatedEpochAuthority) advance() uint64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.epoch++
	return a.epoch
}

type durableAllocationStore struct {
	dir         string
	journal     durableJournal
	remote      *simulatedRemote
	authority   *simulatedEpochAuthority
	writerEpoch uint64
	beforeSave  func(durableJournal) error
	afterRename func() error
	poisoned    bool
}

func initializeDurableStore(dir string, journal durableJournal, pending []byte, remote *simulatedRemote, authority *simulatedEpochAuthority, writerEpoch uint64) (*durableAllocationStore, error) {
	if err := atomicDurableReplace(dir, durablePendingFile, pending); err != nil {
		return nil, err
	}
	store := &durableAllocationStore{dir: dir, remote: remote, authority: authority, writerEpoch: writerEpoch}
	if err := store.save(journal); err != nil {
		return nil, err
	}
	return store, nil
}

func openDurableStore(dir string, remote *simulatedRemote, authority *simulatedEpochAuthority, writerEpoch uint64) (*durableAllocationStore, error) {
	journal, err := readDurableJournal(dir)
	if err != nil {
		return nil, err
	}
	pendingPath := filepath.Join(dir, journal.PendingFile)
	if _, err := os.Stat(pendingPath); err != nil {
		return nil, fmt.Errorf("pending local bytes are unavailable: %w", err)
	}
	return &durableAllocationStore{
		dir:         dir,
		journal:     journal,
		remote:      remote,
		authority:   authority,
		writerEpoch: writerEpoch,
	}, nil
}

func (s *durableAllocationStore) adoptCurrentEpoch(epoch uint64) error {
	if s.poisoned {
		return errDurablePoisoned
	}
	if epoch != s.writerEpoch {
		return errDurableFenced
	}
	if err := s.authority.require(epoch); err != nil {
		return err
	}
	if epoch < s.journal.Epoch {
		return errDurableFenced
	}
	next := cloneDurableJournal(s.journal)
	next.Epoch = epoch
	return s.save(next)
}

func (s *durableAllocationStore) authorize(epoch uint64) error {
	if s.poisoned {
		return errDurablePoisoned
	}
	if epoch != s.writerEpoch || epoch != s.journal.Epoch {
		return errDurableFenced
	}
	return s.authority.require(epoch)
}

func (s *durableAllocationStore) save(next durableJournal) error {
	if err := validateDurableJournal(next); err != nil {
		return err
	}
	encoded, err := json.Marshal(next)
	if err != nil {
		return err
	}
	if len(encoded) > durableJournalMaxBytes {
		return errDurableJournalTooBig
	}
	if s.beforeSave != nil {
		if err := s.beforeSave(next); err != nil {
			s.poisoned = true
			return err
		}
	}
	if err := atomicDurableReplaceAfterRename(s.dir, durableJournalFile, encoded, s.afterRename); err != nil {
		s.poisoned = true
		return err
	}
	s.journal = cloneDurableJournal(next)
	return nil
}

func (s *durableAllocationStore) create(transaction, role string, outcome simulatedCreateOutcome) error {
	epoch := s.writerEpoch
	if err := s.authorize(epoch); err != nil {
		return err
	}
	if transaction == "" || len(transaction) > durableIDMaxBytes || role == "" || len(role) > durableIDMaxBytes {
		return errors.New("invalid generated allocation intent")
	}
	for _, intent := range s.journal.Intents {
		if intent.Operation == "create" && intent.Transaction == transaction {
			return errDurableIntentUnknown
		}
	}
	if s.journal.NextSlot >= s.journal.SlotLimit {
		return errDurableSlotBudget
	}
	slot := s.journal.NextSlot
	next := cloneDurableJournal(s.journal)
	next.NextSlot++
	next.Intents = append(next.Intents, durableIntent{
		Operation:   "create",
		Transaction: transaction,
		Slot:        slot,
		Role:        role,
	})
	// The durable slot intent is committed before the authority permits the
	// simulated remote side effect. A failure here makes no remote call.
	if err := s.save(next); err != nil {
		return err
	}
	var created simulatedRemoteObject
	err := s.authority.run(epoch, func() error {
		var createErr error
		created, createErr = s.remote.create(slot, transaction, role, outcome)
		return createErr
	})
	if err != nil {
		return err
	}
	committed := cloneDurableJournal(s.journal)
	committed.Intents = removeCreateIntent(committed.Intents, transaction)
	committed.Objects = append(committed.Objects, durableObject{ID: created.ID, Slot: slot, Role: role})
	return s.save(committed)
}

func (s *durableAllocationStore) reconcileCreate(transaction string, visibility remoteVisibility) error {
	epoch := s.writerEpoch
	if err := s.authorize(epoch); err != nil {
		return err
	}
	var pending *durableIntent
	for i := range s.journal.Intents {
		intent := &s.journal.Intents[i]
		if intent.Operation == "create" && intent.Transaction == transaction {
			pending = intent
			break
		}
	}
	if pending == nil {
		return errors.New("create intent was not found")
	}
	intent := *pending
	remoteObject, exists, err := s.remote.discover(intent.Slot, visibility)
	if err != nil {
		return err
	}
	next := cloneDurableJournal(s.journal)
	if exists {
		if remoteObject.Transaction != intent.Transaction || remoteObject.Role != intent.Role || remoteObject.Slot != intent.Slot {
			return errDurableJournalCorrupt
		}
		next.Intents = removeCreateIntent(next.Intents, transaction)
		next.Objects = append(next.Objects, durableObject{ID: remoteObject.ID, Slot: intent.Slot, Role: intent.Role})
		return s.save(next)
	}
	next.Intents = removeCreateIntent(next.Intents, transaction)
	if err := s.save(next); err != nil {
		return err
	}
	return errDurableRemoteAbsent
}

// cleanupStep scans at most maxWork inventory entries. A delete intent is
// fsynced before the simulated delete. An unresolved intent consumes one step
// and is never replayed by this method.
func (s *durableAllocationStore) cleanupStep(maxWork int, outcome simulatedDeleteOutcome) (int, error) {
	epoch := s.writerEpoch
	if err := s.authorize(epoch); err != nil {
		return 0, err
	}
	if !s.journal.GraphComplete {
		return 0, errDurableGraphIncomplete
	}
	if maxWork < 1 || maxWork > durableCleanupStepLimit {
		return 0, fmt.Errorf("cleanup step bound must be from 1 to %d", durableCleanupStepLimit)
	}
	work := 0
	for work < maxWork && s.journal.CleanupIndex < uint64(len(s.journal.CleanupIDs)) {
		if err := s.authorize(epoch); err != nil {
			return work, err
		}
		id := s.journal.CleanupIDs[s.journal.CleanupIndex]
		work++
		if _, found := findDeleteIntent(s.journal, id); found {
			return work, errDurableIntentUnknown
		}
		_, protected := protectedDurableIDs(s.journal)[id]
		_, referenced := referencedDurableIDs(s.journal)[id]
		if protected || referenced {
			if err := s.advanceCleanupCursor(epoch); err != nil {
				return work, err
			}
			continue
		}
		object, exists := findDurableObject(s.journal, id)
		if !exists {
			if err := s.advanceCleanupCursor(epoch); err != nil {
				return work, err
			}
			continue
		}
		next := cloneDurableJournal(s.journal)
		next.Intents = append(next.Intents, durableIntent{
			Operation:   "delete",
			Transaction: "delete-" + id,
			ObjectID:    object.ID,
			Slot:        object.Slot,
		})
		if err := s.save(next); err != nil {
			return work, err
		}
		remoteObject, present := s.remote.objects[object.Slot]
		if !present || remoteObject.ID != object.ID {
			return work, errors.New("delete candidate had no matching simulated remote object")
		}
		err := s.authority.run(epoch, func() error {
			return s.remote.delete(object.Slot, object.ID, outcome)
		})
		if err != nil {
			return work, err
		}
		completed := cloneDurableJournal(s.journal)
		completed.Intents = removeDeleteIntent(completed.Intents, object.ID)
		completed.Objects = removeDurableObject(completed.Objects, object.ID)
		completed.CleanupIndex++
		if err := s.save(completed); err != nil {
			return work, err
		}
	}
	return work, nil
}

func (s *durableAllocationStore) reconcileDelete(id string, visibility remoteVisibility) error {
	epoch := s.writerEpoch
	if err := s.authorize(epoch); err != nil {
		return err
	}
	intent, found := findDeleteIntent(s.journal, id)
	if !found {
		return errors.New("delete intent was not found")
	}
	remoteObject, exists, err := s.remote.discover(intent.Slot, visibility)
	if err != nil {
		return err
	}
	if exists {
		if remoteObject.ID != id || remoteObject.Slot != intent.Slot {
			return errDurableJournalCorrupt
		}
		return errDurableRemotePresent
	}
	next := cloneDurableJournal(s.journal)
	next.Intents = removeDeleteIntent(next.Intents, id)
	next.Objects = removeDurableObject(next.Objects, id)
	if next.CleanupIndex < uint64(len(next.CleanupIDs)) && next.CleanupIDs[next.CleanupIndex] == id {
		next.CleanupIndex++
	}
	return s.save(next)
}

func (s *durableAllocationStore) advanceCleanupCursor(epoch uint64) error {
	if err := s.authorize(epoch); err != nil {
		return err
	}
	next := cloneDurableJournal(s.journal)
	next.CleanupIndex++
	return s.save(next)
}

func (s *durableAllocationStore) occupiedSlots() int {
	slots := make(map[uint64]struct{})
	for _, object := range s.journal.Objects {
		slots[object.Slot] = struct{}{}
	}
	for _, intent := range s.journal.Intents {
		if intent.Operation == "create" {
			slots[intent.Slot] = struct{}{}
		}
	}
	return len(slots)
}

func protectedDurableIDs(journal durableJournal) map[string]struct{} {
	objects := make(map[string]durableObject, len(journal.Objects))
	for _, object := range journal.Objects {
		objects[object.ID] = object
	}
	protected := make(map[string]struct{})
	stack := make([]string, 0, 2+len(journal.ReaderPins))
	if journal.CurrentRoot != "" {
		stack = append(stack, journal.CurrentRoot)
	}
	if journal.PreviousRoot != "" {
		stack = append(stack, journal.PreviousRoot)
	}
	stack = append(stack, journal.ReaderPins...)
	for len(stack) > 0 {
		id := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if _, seen := protected[id]; seen {
			continue
		}
		protected[id] = struct{}{}
		if object, exists := objects[id]; exists {
			stack = append(stack, object.References...)
		}
	}
	return protected
}

func referencedDurableIDs(journal durableJournal) map[string]struct{} {
	referenced := make(map[string]struct{})
	for _, object := range journal.Objects {
		for _, id := range object.References {
			referenced[id] = struct{}{}
		}
	}
	return referenced
}

func findDurableObject(journal durableJournal, id string) (durableObject, bool) {
	for _, object := range journal.Objects {
		if object.ID == id {
			return object, true
		}
	}
	return durableObject{}, false
}

func findDeleteIntent(journal durableJournal, id string) (durableIntent, bool) {
	for _, intent := range journal.Intents {
		if intent.Operation == "delete" && intent.ObjectID == id {
			return intent, true
		}
	}
	return durableIntent{}, false
}

func removeCreateIntent(intents []durableIntent, transaction string) []durableIntent {
	return filterDurableIntents(intents, func(intent durableIntent) bool {
		return intent.Operation != "create" || intent.Transaction != transaction
	})
}

func removeDeleteIntent(intents []durableIntent, id string) []durableIntent {
	return filterDurableIntents(intents, func(intent durableIntent) bool {
		return intent.Operation != "delete" || intent.ObjectID != id
	})
}

func filterDurableIntents(intents []durableIntent, keep func(durableIntent) bool) []durableIntent {
	filtered := make([]durableIntent, 0, len(intents))
	for _, intent := range intents {
		if keep(intent) {
			filtered = append(filtered, intent)
		}
	}
	return filtered
}

func removeDurableObject(objects []durableObject, id string) []durableObject {
	filtered := make([]durableObject, 0, len(objects))
	for _, object := range objects {
		if object.ID != id {
			filtered = append(filtered, object)
		}
	}
	return filtered
}

func cloneDurableJournal(journal durableJournal) durableJournal {
	clone := journal
	clone.ReaderPins = append([]string(nil), journal.ReaderPins...)
	clone.Objects = append([]durableObject(nil), journal.Objects...)
	for i := range clone.Objects {
		clone.Objects[i].References = append([]string(nil), journal.Objects[i].References...)
	}
	clone.Intents = append([]durableIntent(nil), journal.Intents...)
	clone.CleanupIDs = append([]string(nil), journal.CleanupIDs...)
	return clone
}

func validateDurableJournal(journal durableJournal) error {
	if journal.Version != durableJournalVersion || journal.SlotLimit != durableSlotLimit || journal.Epoch == 0 || journal.PendingFile != durablePendingFile {
		return errDurableJournalCorrupt
	}
	if journal.NextSlot > journal.SlotLimit || len(journal.Objects)+len(journal.Intents) > durableJournalEntryLimit || len(journal.CleanupIDs) > durableSlotLimit || journal.CleanupIndex > uint64(len(journal.CleanupIDs)) {
		return errDurableJournalCorrupt
	}
	objects := make(map[string]durableObject, len(journal.Objects))
	objectSlots := make(map[uint64]string, len(journal.Objects))
	for _, object := range journal.Objects {
		if object.ID == "" || len(object.ID) > durableIDMaxBytes || object.Role == "" || len(object.Role) > durableIDMaxBytes || object.Slot >= journal.NextSlot || len(object.References) > durableSlotLimit {
			return errDurableJournalCorrupt
		}
		if _, duplicate := objects[object.ID]; duplicate {
			return errDurableJournalCorrupt
		}
		if _, duplicate := objectSlots[object.Slot]; duplicate {
			return errDurableJournalCorrupt
		}
		objects[object.ID] = object
		objectSlots[object.Slot] = object.ID
		for _, reference := range object.References {
			if reference == "" || len(reference) > durableIDMaxBytes {
				return errDurableJournalCorrupt
			}
		}
	}
	for _, object := range journal.Objects {
		for _, reference := range object.References {
			if _, exists := objects[reference]; !exists {
				return errDurableJournalCorrupt
			}
		}
	}
	createTransactions := make(map[string]struct{})
	deleteTargets := make(map[string]struct{})
	createSlots := make(map[uint64]struct{})
	for _, intent := range journal.Intents {
		if intent.Transaction == "" || len(intent.Transaction) > durableIDMaxBytes || intent.Slot >= journal.NextSlot {
			return errDurableJournalCorrupt
		}
		switch intent.Operation {
		case "create":
			if intent.Role == "" || len(intent.Role) > durableIDMaxBytes || intent.ObjectID != "" {
				return errDurableJournalCorrupt
			}
			if _, duplicate := createTransactions[intent.Transaction]; duplicate {
				return errDurableJournalCorrupt
			}
			if _, duplicate := createSlots[intent.Slot]; duplicate {
				return errDurableJournalCorrupt
			}
			if _, duplicate := objectSlots[intent.Slot]; duplicate {
				return errDurableJournalCorrupt
			}
			createTransactions[intent.Transaction] = struct{}{}
			createSlots[intent.Slot] = struct{}{}
		case "delete":
			object, exists := objects[intent.ObjectID]
			if !exists || object.Slot != intent.Slot {
				return errDurableJournalCorrupt
			}
			if _, duplicate := deleteTargets[intent.ObjectID]; duplicate {
				return errDurableJournalCorrupt
			}
			deleteTargets[intent.ObjectID] = struct{}{}
		default:
			return errDurableJournalCorrupt
		}
	}
	if uint64(len(objectSlots)+len(createSlots)) > journal.SlotLimit {
		return errDurableJournalCorrupt
	}
	if journal.CurrentRoot != "" {
		if root, exists := objects[journal.CurrentRoot]; !exists || root.Role != "root" {
			return errDurableJournalCorrupt
		}
	}
	if journal.PreviousRoot != "" {
		if root, exists := objects[journal.PreviousRoot]; !exists || root.Role != "root" {
			return errDurableJournalCorrupt
		}
	}
	for _, pin := range journal.ReaderPins {
		if root, exists := objects[pin]; !exists || root.Role != "root" {
			return errDurableJournalCorrupt
		}
	}
	for _, id := range journal.CleanupIDs {
		if id == "" || len(id) > durableIDMaxBytes {
			return errDurableJournalCorrupt
		}
	}
	return nil
}

func readDurableJournal(dir string) (durableJournal, error) {
	path := filepath.Join(dir, durableJournalFile)
	file, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return durableJournal{}, errDurableJournalMissing
		}
		return durableJournal{}, err
	}
	defer func() { _ = file.Close() }()
	payload, err := io.ReadAll(io.LimitReader(file, durableJournalMaxBytes+1))
	if err != nil {
		return durableJournal{}, err
	}
	if len(payload) > durableJournalMaxBytes {
		return durableJournal{}, errDurableJournalTooBig
	}
	var journal durableJournal
	if err := json.Unmarshal(payload, &journal); err != nil {
		return durableJournal{}, fmt.Errorf("%w: %v", errDurableJournalCorrupt, err)
	}
	if err := validateDurableJournal(journal); err != nil {
		return durableJournal{}, err
	}
	return journal, nil
}

// atomicDurableReplace exercises file fsync, rename, and parent-directory
// fsync. A single fixed temp path bounds leftovers after an interrupted write.
// Journal replacement peaks at 32 KiB across the old file and temp file.
func atomicDurableReplace(dir, name string, payload []byte) error {
	return atomicDurableReplaceAfterRename(dir, name, payload, nil)
}

func atomicDurableReplaceAfterRename(dir, name string, payload []byte, afterRename func() error) error {
	if name != durableJournalFile && name != durablePendingFile {
		return errors.New("unexpected generated durable filename")
	}
	if name == durableJournalFile && len(payload) > durableJournalMaxBytes {
		return errDurableJournalTooBig
	}
	temporaryName := filepath.Join(dir, "."+name+".tmp")
	temporary, err := os.OpenFile(temporaryName, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	if err := temporary.Chmod(0600); err != nil {
		_ = temporary.Close()
		return err
	}
	if n, err := temporary.Write(payload); err != nil {
		_ = temporary.Close()
		return err
	} else if n != len(payload) {
		_ = temporary.Close()
		return io.ErrShortWrite
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryName, filepath.Join(dir, name)); err != nil {
		return err
	}
	if afterRename != nil {
		if err := afterRename(); err != nil {
			return err
		}
	}
	directory, err := os.Open(dir)
	if err != nil {
		return err
	}
	syncErr := directory.Sync()
	closeErr := directory.Close()
	if syncErr != nil {
		return syncErr
	}
	return closeErr
}

func durableJournalWithObjects(epoch uint64, objects []durableObject, cleanupIDs []string) durableJournal {
	return durableJournal{
		Version:       durableJournalVersion,
		Epoch:         epoch,
		SlotLimit:     durableSlotLimit,
		NextSlot:      uint64(len(objects)),
		PendingFile:   durablePendingFile,
		GraphComplete: true,
		Objects:       append([]durableObject(nil), objects...),
		CleanupIDs:    append([]string(nil), cleanupIDs...),
	}
}

func remoteFixtureFor(objects []durableObject) *simulatedRemote {
	remote := newSimulatedRemote()
	for _, object := range objects {
		remote.objects[object.Slot] = simulatedRemoteObject{ID: object.ID, Role: object.Role, Slot: object.Slot}
	}
	return remote
}

func createIntentDiskCheck(dir string) func(uint64, string) error {
	return func(slot uint64, transaction string) error {
		journal, err := readDurableJournal(dir)
		if err != nil {
			return err
		}
		for _, intent := range journal.Intents {
			if intent.Operation == "create" && intent.Transaction == transaction && intent.Slot == slot {
				return nil
			}
		}
		return errors.New("on-disk journal did not contain the create intent")
	}
}

func deleteIntentDiskCheck(dir string) func(uint64, string) error {
	return func(slot uint64, id string) error {
		journal, err := readDurableJournal(dir)
		if err != nil {
			return err
		}
		for _, intent := range journal.Intents {
			if intent.Operation == "delete" && intent.ObjectID == id && intent.Slot == slot {
				return nil
			}
		}
		return errors.New("on-disk journal did not contain the delete intent")
	}
}

func TestScalabilityDurableCreateIntentAndRestart(t *testing.T) {
	dir := t.TempDir()
	authority := newSimulatedEpochAuthority(1)
	remote := newSimulatedRemote()
	pending := bytes.Repeat([]byte{0xA7}, 4096)
	initial := durableJournalWithObjects(1, nil, nil)
	_, err := initializeDurableStore(dir, initial, pending, remote, authority, 1)
	if err != nil {
		t.Fatal(err)
	}
	journalTemp := filepath.Join(dir, "."+durableJournalFile+".tmp")
	if err := os.WriteFile(journalTemp, bytes.Repeat([]byte{0xCC}, durableJournalMaxBytes), 0600); err != nil {
		t.Fatal(err)
	}
	store, err := openDurableStore(dir, remote, authority, 1)
	if err != nil {
		t.Fatal(err)
	}
	remote.beforeCreate = createIntentDiskCheck(dir)
	checkpointFailure := errors.New("injected journal checkpoint failure")
	store.beforeSave = func(next durableJournal) error {
		for _, intent := range next.Intents {
			if intent.Operation == "create" && intent.Transaction == "txn-checkpoint-fail" {
				return checkpointFailure
			}
		}
		return nil
	}
	if err := store.create("txn-checkpoint-fail", "directory", createAckApplied); !errors.Is(err, checkpointFailure) {
		t.Fatalf("intent checkpoint failure: %v", err)
	}
	if remote.createCalls != 0 || store.journal.NextSlot != 0 {
		t.Fatalf("remote create ran without durable intent: calls=%d next=%d", remote.createCalls, store.journal.NextSlot)
	}
	if err := store.create("txn-after-checkpoint-fail", "directory", createAckApplied); !errors.Is(err, errDurablePoisoned) {
		t.Fatalf("store continued after a failed checkpoint: %v", err)
	}
	if info, err := os.Stat(journalTemp); err != nil || info.Size() != durableJournalMaxBytes {
		t.Fatalf("bounded abandoned temp was not retained: stat=%v info=%v", err, info)
	}
	// Reopen discards in-memory uncertainty and safely reuses the one fixed temp.
	store, err = openDurableStore(dir, remote, authority, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.create("txn-create-1", "directory", createAckLostAfterApply); !errors.Is(err, errDurableAckLost) {
		t.Fatalf("lost create acknowledgement: %v", err)
	}
	if remote.createCalls != 1 || store.occupiedSlots() != 1 || store.journal.NextSlot != 1 || len(store.journal.Intents) != 1 {
		t.Fatalf("ambiguous create did not reserve its slot: calls=%d occupied=%d next=%d intents=%d", remote.createCalls, store.occupiedSlots(), store.journal.NextSlot, len(store.journal.Intents))
	}
	info, err := os.Stat(filepath.Join(dir, durableJournalFile))
	if err != nil || info.Size() > durableJournalMaxBytes {
		t.Fatalf("journal byte bound: stat=%v info=%v", err, info)
	}

	// A new store instance reopens real files. The pending file is separate from
	// the metadata journal and remains intact across the simulated restart.
	reopened, err := openDurableStore(dir, remote, authority, 1)
	if err != nil {
		t.Fatal(err)
	}
	recoveredPending, err := os.ReadFile(filepath.Join(dir, reopened.journal.PendingFile))
	if err != nil || !bytes.Equal(recoveredPending, pending) {
		t.Fatalf("pending local bytes did not survive reopen: read=%v equal=%v", err, bytes.Equal(recoveredPending, pending))
	}
	if err := reopened.create("txn-create-1", "directory", createAckApplied); !errors.Is(err, errDurableIntentUnknown) {
		t.Fatalf("unresolved transaction was replayed: %v", err)
	}
	if remote.createCalls != 1 {
		t.Fatalf("blind replay sent another create: calls=%d", remote.createCalls)
	}
	if err := reopened.reconcileCreate("txn-create-1", remoteVisibilityUnknown); !errors.Is(err, errDurableIntentUnknown) {
		t.Fatalf("unknown discovery released the slot: %v", err)
	}
	if reopened.occupiedSlots() != 1 || remote.createCalls != 1 {
		t.Fatalf("unknown discovery changed reservation or retried: occupied=%d calls=%d", reopened.occupiedSlots(), remote.createCalls)
	}
	if err := reopened.reconcileCreate("txn-create-1", remoteVisibilityAuthoritative); err != nil {
		t.Fatalf("authoritative discovery did not reconcile applied create: %v", err)
	}
	if len(reopened.journal.Intents) != 0 || len(reopened.journal.Objects) != 1 || reopened.journal.Objects[0].ID != "remote-0" || remote.createCalls != 1 {
		t.Fatalf("reconciled create state: journal=%+v calls=%d", reopened.journal, remote.createCalls)
	}
	postCreateCheckpoint := errors.New("injected post-create checkpoint failure")
	reopened.beforeSave = func(next durableJournal) error {
		for _, object := range next.Objects {
			if object.ID == "remote-1" {
				return postCreateCheckpoint
			}
		}
		return nil
	}
	if err := reopened.create("txn-create-2", "directory", createAckApplied); !errors.Is(err, postCreateCheckpoint) {
		t.Fatalf("post-create checkpoint failure: %v", err)
	}
	if remote.createCalls != 2 || !hasCreateIntent(reopened.journal, "txn-create-2") {
		t.Fatalf("post-create failure lost its durable intent: calls=%d intents=%+v", remote.createCalls, reopened.journal.Intents)
	}
	if err := reopened.create("txn-create-2", "directory", createAckApplied); !errors.Is(err, errDurablePoisoned) {
		t.Fatalf("post-create failure allowed another side effect: %v", err)
	}
	reopened, err = openDurableStore(dir, remote, authority, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := reopened.create("txn-create-2", "directory", createAckApplied); !errors.Is(err, errDurableIntentUnknown) || remote.createCalls != 2 {
		t.Fatalf("reopened post-create intent was blindly replayed: err=%v calls=%d", err, remote.createCalls)
	}
	if err := reopened.reconcileCreate("txn-create-2", remoteVisibilityAuthoritative); err != nil {
		t.Fatalf("post-create checkpoint recovery: %v", err)
	}

	// A rename completed without its directory-sync checkpoint poisons the old
	// instance. Reopening sees the reserved high-water mark and unresolved intent.
	renameFailure := errors.New("injected parent-directory sync failure")
	reopened.afterRename = func() error { return renameFailure }
	if err := reopened.create("txn-create-3", "directory", createAckApplied); !errors.Is(err, renameFailure) || remote.createCalls != 2 {
		t.Fatalf("post-rename failure allowed remote create: err=%v calls=%d", err, remote.createCalls)
	}
	if err := reopened.create("txn-create-4", "directory", createAckApplied); !errors.Is(err, errDurablePoisoned) || remote.createCalls != 2 {
		t.Fatalf("poisoned store issued another create: err=%v calls=%d", err, remote.createCalls)
	}
	reopened, err = openDurableStore(dir, remote, authority, 1)
	if err != nil {
		t.Fatal(err)
	}
	if reopened.journal.NextSlot != 3 || !hasCreateIntent(reopened.journal, "txn-create-3") || remote.createCalls != 2 {
		t.Fatalf("reopen did not preserve renamed intent/high-water: next=%d intents=%+v calls=%d", reopened.journal.NextSlot, reopened.journal.Intents, remote.createCalls)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 2 {
		t.Fatalf("fixed temp slot accumulated files: entries=%d err=%v", len(entries), err)
	}
	info, err = os.Stat(filepath.Join(dir, durableJournalFile))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("durable create: journal_bytes=%d slots=%d create_calls=%d pending_bytes=%d temp_entries=%d", info.Size(), reopened.occupiedSlots(), remote.createCalls, len(recoveredPending), len(entries))
}

func TestScalabilityDurableJournalFailsClosedAndBoundsRetries(t *testing.T) {
	missingDir := t.TempDir()
	if _, err := openDurableStore(missingDir, newSimulatedRemote(), newSimulatedEpochAuthority(1), 1); !errors.Is(err, errDurableJournalMissing) {
		t.Fatalf("missing journal did not fail closed: %v", err)
	}
	corruptDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(corruptDir, durableJournalFile), []byte("{truncated"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := openDurableStore(corruptDir, newSimulatedRemote(), newSimulatedEpochAuthority(1), 1); !errors.Is(err, errDurableJournalCorrupt) {
		t.Fatalf("corrupt journal did not fail closed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(corruptDir, durableJournalFile)); err != nil {
		t.Fatalf("failed-open path replaced corrupt safety state: %v", err)
	}
	danglingDir := t.TempDir()
	dangling := durableJournalWithObjects(1, []durableObject{{ID: "root-dangling", Slot: 0, Role: "root", References: []string{"missing-data"}}}, []string{"root-dangling"})
	dangling.CurrentRoot = "root-dangling"
	encoded, err := json.Marshal(dangling)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(danglingDir, durablePendingFile), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(danglingDir, durableJournalFile), encoded, 0600); err != nil {
		t.Fatal(err)
	}
	danglingRemote := remoteFixtureFor(dangling.Objects)
	if _, err := openDurableStore(danglingDir, danglingRemote, newSimulatedEpochAuthority(1), 1); !errors.Is(err, errDurableJournalCorrupt) {
		t.Fatalf("dangling reference did not fail closed: %v", err)
	}
	if danglingRemote.deleteCalls != 0 {
		t.Fatal("incomplete reference inventory issued a delete")
	}

	dir := t.TempDir()
	authority := newSimulatedEpochAuthority(1)
	remote := newSimulatedRemote()
	store, err := initializeDurableStore(dir, durableJournalWithObjects(1, nil, nil), nil, remote, authority, 1)
	if err != nil {
		t.Fatal(err)
	}
	remote.beforeCreate = createIntentDiskCheck(dir)
	for attempt := 0; attempt < durableSlotLimit; attempt++ {
		if attempt > 0 {
			store, err = openDurableStore(dir, remote, authority, 1)
			if err != nil {
				t.Fatal(err)
			}
		}
		tx := fmt.Sprintf("txn-failed-%d", attempt)
		if err := store.create(tx, "metadata", createAckLostBeforeApply); !errors.Is(err, errDurableAckLost) {
			t.Fatalf("attempt %d lost acknowledgement: %v", attempt, err)
		}
		if store.occupiedSlots() != attempt+1 || store.journal.NextSlot != uint64(attempt+1) {
			t.Fatalf("attempt %d did not persist slot high-water: occupied=%d next=%d", attempt, store.occupiedSlots(), store.journal.NextSlot)
		}
		info, err := os.Stat(filepath.Join(dir, durableJournalFile))
		if err != nil || info.Size() > durableJournalMaxBytes {
			t.Fatalf("attempt %d journal exceeded byte bound: stat=%v info=%v", attempt, err, info)
		}
	}
	store, err = openDurableStore(dir, remote, authority, 1)
	if err != nil {
		t.Fatal(err)
	}
	beforeCalls := remote.createCalls
	if err := store.create("txn-over-budget", "metadata", createAckApplied); !errors.Is(err, errDurableSlotBudget) {
		t.Fatalf("repeated failures did not exhaust bounded slot budget: %v", err)
	}
	if remote.createCalls != beforeCalls || store.occupiedSlots() != durableSlotLimit || store.journal.NextSlot != durableSlotLimit {
		t.Fatalf("budget rejection changed remote or allocation state: calls=%d occupied=%d next=%d", remote.createCalls, store.occupiedSlots(), store.journal.NextSlot)
	}
	info, err := os.Stat(filepath.Join(dir, durableJournalFile))
	if err != nil || info.Size() > durableJournalMaxBytes || len(store.journal.Intents) > durableSlotLimit {
		t.Fatalf("bounded journal assertions failed: stat=%v size=%v intents=%d", err, info, len(store.journal.Intents))
	}
	t.Logf("repeated lost creates: attempts=%d reserved_slots=%d journal_bytes=%d max_bytes=%d create_calls=%d", durableSlotLimit, store.occupiedSlots(), info.Size(), durableJournalMaxBytes, remote.createCalls)
}

func TestScalabilityDurableCleanupPinsCursorAndUnknownDelete(t *testing.T) {
	dir := t.TempDir()
	authority := newSimulatedEpochAuthority(1)
	objects := []durableObject{
		{ID: "root-current", Slot: 0, Role: "root", References: []string{"data-current"}},
		{ID: "root-previous", Slot: 1, Role: "root", References: []string{"data-previous"}},
		{ID: "root-reader", Slot: 2, Role: "root", References: []string{"data-reader"}},
		{ID: "data-current", Slot: 3, Role: "file-data"},
		{ID: "data-previous", Slot: 4, Role: "file-data"},
		{ID: "data-reader", Slot: 5, Role: "file-data"},
		{ID: "orphan-meta", Slot: 6, Role: "metadata"},
		{ID: "orphan-checkpoint", Slot: 7, Role: "metadata"},
	}
	journal := durableJournalWithObjects(1, objects, []string{
		"data-current", "data-previous", "data-reader", "orphan-meta", "orphan-checkpoint",
		"root-current", "root-previous", "root-reader",
	})
	journal.CurrentRoot = "root-current"
	journal.PreviousRoot = "root-previous"
	journal.ReaderPins = []string{"root-reader"}
	remote := remoteFixtureFor(objects)
	store, err := initializeDurableStore(dir, journal, []byte("pending local version"), remote, authority, 1)
	if err != nil {
		t.Fatal(err)
	}
	remote.beforeDelete = deleteIntentDiskCheck(dir)
	work, err := store.cleanupStep(2, deleteAckApplied)
	if err != nil || work != 2 || store.journal.CleanupIndex != 2 || remote.deleteCalls != 0 {
		t.Fatalf("first bounded cleanup checkpoint: work=%d err=%v cursor=%d deletes=%d", work, err, store.journal.CleanupIndex, remote.deleteCalls)
	}

	// Reopening reconstructs the coordinator from the disk journal; the cursor
	// starts at the third entry rather than rescanning or resetting to zero.
	store, err = openDurableStore(dir, remote, authority, 1)
	if err != nil {
		t.Fatal(err)
	}
	store.beforeSave = func(next durableJournal) error {
		if hasDeleteIntent(next, "orphan-meta") {
			return errors.New("injected delete-intent checkpoint failure")
		}
		return nil
	}
	work, err = store.cleanupStep(2, deleteAckLostAfterApply)
	if err == nil || work != 2 || store.journal.CleanupIndex != 3 || remote.deleteCalls != 0 {
		t.Fatalf("delete ran without durable intent: work=%d err=%v cursor=%d deletes=%d", work, err, store.journal.CleanupIndex, remote.deleteCalls)
	}
	store, err = openDurableStore(dir, remote, authority, 1)
	if err != nil {
		t.Fatal(err)
	}
	work, err = store.cleanupStep(2, deleteAckLostAfterApply)
	if !errors.Is(err, errDurableAckLost) || work != 1 || store.journal.CleanupIndex != 3 || remote.deleteCalls != 1 {
		t.Fatalf("unknown delete boundary: work=%d err=%v cursor=%d deletes=%d", work, err, store.journal.CleanupIndex, remote.deleteCalls)
	}
	if store.occupiedSlots() != len(objects) || !hasDeleteIntent(store.journal, "orphan-meta") {
		t.Fatalf("unknown delete reclaimed its reservation: occupied=%d intents=%+v", store.occupiedSlots(), store.journal.Intents)
	}
	if _, exists := remote.objects[6]; exists {
		t.Fatal("lost acknowledgement fixture did not apply remote delete")
	}

	store, err = openDurableStore(dir, remote, authority, 1)
	if err != nil {
		t.Fatal(err)
	}
	beforeDeleteCalls := remote.deleteCalls
	work, err = store.cleanupStep(2, deleteAckApplied)
	if !errors.Is(err, errDurableIntentUnknown) || work != 1 || remote.deleteCalls != beforeDeleteCalls || store.journal.CleanupIndex != 3 {
		t.Fatalf("restart blindly retried unresolved delete: work=%d err=%v cursor=%d deletes=%d", work, err, store.journal.CleanupIndex, remote.deleteCalls)
	}
	if err := store.reconcileDelete("orphan-meta", remoteVisibilityUnknown); !errors.Is(err, errDurableIntentUnknown) {
		t.Fatalf("uncertain visibility reclaimed delete slot: %v", err)
	}
	if store.occupiedSlots() != len(objects) {
		t.Fatalf("unknown visibility released slot: %d", store.occupiedSlots())
	}
	if err := store.reconcileDelete("orphan-meta", remoteVisibilityAuthoritative); err != nil {
		t.Fatalf("authoritative absence did not complete delete: %v", err)
	}
	if store.occupiedSlots() != len(objects)-1 || hasDeleteIntent(store.journal, "orphan-meta") || store.journal.CleanupIndex != 4 {
		t.Fatalf("confirmed absence did not release local reservation: occupied=%d intents=%+v cursor=%d", store.occupiedSlots(), store.journal.Intents, store.journal.CleanupIndex)
	}
	store.beforeSave = func(next durableJournal) error {
		if hasDeleteIntent(next, "orphan-checkpoint") {
			return errors.New("injected delete-intent checkpoint failure")
		}
		return nil
	}
	work, err = store.cleanupStep(1, deleteAckApplied)
	if err == nil || work != 1 || store.journal.CleanupIndex != 4 || remote.deleteCalls != 1 {
		t.Fatalf("delete ran without durable intent checkpoint: work=%d err=%v cursor=%d deletes=%d", work, err, store.journal.CleanupIndex, remote.deleteCalls)
	}
	store, err = openDurableStore(dir, remote, authority, 1)
	if err != nil {
		t.Fatal(err)
	}
	postDeleteCheckpoint := errors.New("injected post-delete checkpoint failure")
	store.beforeSave = func(next durableJournal) error {
		_, stillPresent := findDurableObject(next, "orphan-checkpoint")
		if !stillPresent && !hasDeleteIntent(next, "orphan-checkpoint") {
			return postDeleteCheckpoint
		}
		return nil
	}
	work, err = store.cleanupStep(1, deleteAckApplied)
	if !errors.Is(err, postDeleteCheckpoint) || work != 1 || remote.deleteCalls != 2 || !hasDeleteIntent(store.journal, "orphan-checkpoint") {
		t.Fatalf("post-delete checkpoint failure: work=%d err=%v deletes=%d intents=%+v", work, err, remote.deleteCalls, store.journal.Intents)
	}
	store, err = openDurableStore(dir, remote, authority, 1)
	if err != nil {
		t.Fatal(err)
	}
	beforeDeleteCalls = remote.deleteCalls
	work, err = store.cleanupStep(1, deleteAckApplied)
	if !errors.Is(err, errDurableIntentUnknown) || work != 1 || remote.deleteCalls != beforeDeleteCalls {
		t.Fatalf("restart replayed post-delete checkpoint: work=%d err=%v deletes=%d", work, err, remote.deleteCalls)
	}
	if err := store.reconcileDelete("orphan-checkpoint", remoteVisibilityUnknown); !errors.Is(err, errDurableIntentUnknown) {
		t.Fatalf("post-delete unknown visibility reclaimed slot: %v", err)
	}
	if err := store.reconcileDelete("orphan-checkpoint", remoteVisibilityAuthoritative); err != nil {
		t.Fatalf("post-delete authoritative absence: %v", err)
	}
	if store.journal.CleanupIndex != 5 || store.occupiedSlots() != len(objects)-2 {
		t.Fatalf("post-delete reconciliation did not advance: cursor=%d occupied=%d", store.journal.CleanupIndex, store.occupiedSlots())
	}

	for _, id := range []string{"root-current", "root-previous", "root-reader", "data-current", "data-previous", "data-reader"} {
		object, exists := findDurableObject(store.journal, id)
		if !exists {
			t.Fatalf("cleanup removed protected object %q from journal", id)
		}
		if remoteObject, exists := remote.objects[object.Slot]; !exists || remoteObject.ID != id {
			t.Fatalf("cleanup removed protected remote object %q", id)
		}
	}
	work, err = store.cleanupStep(2, deleteAckApplied)
	if err != nil || work != 2 || store.journal.CleanupIndex != 7 || remote.deleteCalls != 2 {
		t.Fatalf("remaining root cleanup bound: work=%d err=%v cursor=%d deletes=%d", work, err, store.journal.CleanupIndex, remote.deleteCalls)
	}
	store, err = openDurableStore(dir, remote, authority, 1)
	if err != nil {
		t.Fatal(err)
	}
	work, err = store.cleanupStep(2, deleteAckApplied)
	if err != nil || work != 1 || store.journal.CleanupIndex != uint64(len(store.journal.CleanupIDs)) || remote.deleteCalls != 2 {
		t.Fatalf("final pinned cleanup checkpoint: work=%d err=%v cursor=%d deletes=%d", work, err, store.journal.CleanupIndex, remote.deleteCalls)
	}
	beforeCreateCalls := remote.createCalls
	if err := store.create("txn-after-delete-budget", "metadata", createAckApplied); !errors.Is(err, errDurableSlotBudget) || remote.createCalls != beforeCreateCalls {
		t.Fatalf("delete incorrectly restored lifetime allocation capacity: err=%v creates=%d", err, remote.createCalls)
	}
	t.Logf("cleanup: per_step_limit=%d cursor=%d confirmed_deletes=%d reserved_after_delete=%d", durableCleanupStepLimit, store.journal.CleanupIndex, remote.deleteCalls, store.occupiedSlots())
}

func TestScalabilityDurableCleanupRequiresCompleteGraph(t *testing.T) {
	dir := t.TempDir()
	authority := newSimulatedEpochAuthority(1)
	objects := []durableObject{{ID: "orphan-in-incomplete-graph", Slot: 0, Role: "metadata"}}
	journal := durableJournalWithObjects(1, objects, []string{"orphan-in-incomplete-graph"})
	journal.GraphComplete = false
	remote := remoteFixtureFor(objects)
	store, err := initializeDurableStore(dir, journal, nil, remote, authority, 1)
	if err != nil {
		t.Fatal(err)
	}
	remote.beforeDelete = deleteIntentDiskCheck(dir)
	work, err := store.cleanupStep(1, deleteAckApplied)
	if !errors.Is(err, errDurableGraphIncomplete) || work != 0 || store.journal.CleanupIndex != 0 || remote.deleteCalls != 0 {
		t.Fatalf("incomplete graph admitted cleanup: work=%d err=%v cursor=%d deletes=%d", work, err, store.journal.CleanupIndex, remote.deleteCalls)
	}
}

func TestScalabilityDurableUnknownDeletePresentDoesNotReplay(t *testing.T) {
	dir := t.TempDir()
	authority := newSimulatedEpochAuthority(1)
	objects := []durableObject{{ID: "orphan-still-present", Slot: 0, Role: "metadata"}}
	journal := durableJournalWithObjects(1, objects, []string{"orphan-still-present"})
	remote := remoteFixtureFor(objects)
	store, err := initializeDurableStore(dir, journal, nil, remote, authority, 1)
	if err != nil {
		t.Fatal(err)
	}
	remote.beforeDelete = deleteIntentDiskCheck(dir)
	work, err := store.cleanupStep(1, deleteAckLostBeforeApply)
	if !errors.Is(err, errDurableAckLost) || work != 1 || remote.deleteCalls != 1 || !hasDeleteIntent(store.journal, "orphan-still-present") {
		t.Fatalf("pre-apply lost delete did not retain intent: work=%d err=%v calls=%d intents=%+v", work, err, remote.deleteCalls, store.journal.Intents)
	}
	store, err = openDurableStore(dir, remote, authority, 1)
	if err != nil {
		t.Fatal(err)
	}
	work, err = store.cleanupStep(1, deleteAckApplied)
	if !errors.Is(err, errDurableIntentUnknown) || work != 1 || remote.deleteCalls != 1 {
		t.Fatalf("restart replayed delete with present outcome: work=%d err=%v calls=%d", work, err, remote.deleteCalls)
	}
	if err := store.reconcileDelete("orphan-still-present", remoteVisibilityAuthoritative); !errors.Is(err, errDurableRemotePresent) {
		t.Fatalf("authoritative presence released unresolved delete: %v", err)
	}
	if !hasDeleteIntent(store.journal, "orphan-still-present") || store.occupiedSlots() != 1 || remote.deleteCalls != 1 {
		t.Fatalf("present object lost its reservation or was replayed: intents=%+v occupied=%d calls=%d", store.journal.Intents, store.occupiedSlots(), remote.deleteCalls)
	}
	// No deliberate retry protocol is modeled; this safety case remains pending.
}

func TestScalabilityDurableCleanupMayRetainChildBeforeParent(t *testing.T) {
	dir := t.TempDir()
	authority := newSimulatedEpochAuthority(1)
	objects := []durableObject{
		{ID: "orphan-child", Slot: 0, Role: "metadata"},
		{ID: "orphan-parent", Slot: 1, Role: "metadata", References: []string{"orphan-child"}},
	}
	journal := durableJournalWithObjects(1, objects, []string{"orphan-child", "orphan-parent"})
	remote := remoteFixtureFor(objects)
	store, err := initializeDurableStore(dir, journal, nil, remote, authority, 1)
	if err != nil {
		t.Fatal(err)
	}
	remote.beforeDelete = deleteIntentDiskCheck(dir)
	work, err := store.cleanupStep(2, deleteAckApplied)
	if err != nil || work != 2 || store.journal.CleanupIndex != 2 || remote.deleteCalls != 1 {
		t.Fatalf("bounded child-before-parent sweep: work=%d err=%v cursor=%d deletes=%d", work, err, store.journal.CleanupIndex, remote.deleteCalls)
	}
	if _, exists := findDurableObject(store.journal, "orphan-child"); !exists {
		t.Fatal("one-pass cleanup removed a child before its unprotected parent")
	}
	if _, exists := findDurableObject(store.journal, "orphan-parent"); exists {
		t.Fatal("unreferenced parent was not eligible for cleanup")
	}
	// The child remains a safe orphan after this cursor finishes. A later sweep
	// is outside this bounded fixture; unique slot ordinals are never reused.
}

func TestScalabilityDurableStaleEpochRejectsAllocationAndDelete(t *testing.T) {
	dir := t.TempDir()
	authority := newSimulatedEpochAuthority(1)
	objects := []durableObject{{ID: "orphan-stale", Slot: 0, Role: "metadata"}}
	journal := durableJournalWithObjects(1, objects, []string{"orphan-stale"})
	remote := remoteFixtureFor(objects)
	staleStore, err := initializeDurableStore(dir, journal, nil, remote, authority, 1)
	if err != nil {
		t.Fatal(err)
	}
	remote.beforeCreate = createIntentDiskCheck(dir)
	newEpoch := authority.advance()
	if newEpoch != 2 {
		t.Fatalf("unexpected simulated authority epoch: %d", newEpoch)
	}
	beforeCreateCalls := remote.createCalls
	if err := staleStore.create("txn-stale-create", "directory", createAckApplied); !errors.Is(err, errDurableFenced) {
		t.Fatalf("stale allocation was accepted after takeover: %v", err)
	}
	if remote.createCalls != beforeCreateCalls || staleStore.journal.NextSlot != 1 {
		t.Fatalf("stale allocation changed state: calls=%d next=%d", remote.createCalls, staleStore.journal.NextSlot)
	}
	work, err := staleStore.cleanupStep(1, deleteAckApplied)
	if !errors.Is(err, errDurableFenced) || work != 0 || remote.deleteCalls != 0 {
		t.Fatalf("stale cleanup/delete was accepted after takeover: work=%d err=%v deletes=%d", work, err, remote.deleteCalls)
	}

	currentStore, err := openDurableStore(dir, remote, authority, newEpoch)
	if err != nil {
		t.Fatal(err)
	}
	if err := currentStore.adoptCurrentEpoch(newEpoch); err != nil {
		t.Fatalf("new epoch could not adopt the journal: %v", err)
	}
	work, err = currentStore.cleanupStep(1, deleteAckApplied)
	if err != nil || work != 1 || remote.deleteCalls != 1 || len(currentStore.journal.Objects) != 0 {
		t.Fatalf("current epoch could not clean unreferenced object: work=%d err=%v deletes=%d objects=%d", work, err, remote.deleteCalls, len(currentStore.journal.Objects))
	}
	t.Logf("fencing: stale_create_calls=%d stale_delete_calls=0 current_epoch=%d", remote.createCalls, newEpoch)
}

func hasDeleteIntent(journal durableJournal, id string) bool {
	_, exists := findDeleteIntent(journal, id)
	return exists
}

func hasCreateIntent(journal durableJournal, transaction string) bool {
	for _, intent := range journal.Intents {
		if intent.Operation == "create" && intent.Transaction == transaction {
			return true
		}
	}
	return false
}

func TestScalabilityDurableJournalValidationRejectsInvalidBounds(t *testing.T) {
	journal := durableJournalWithObjects(1, nil, nil)
	journal.NextSlot = journal.SlotLimit + 1
	if err := validateDurableJournal(journal); !errors.Is(err, errDurableJournalCorrupt) {
		t.Fatalf("invalid high-water journal passed validation: %v", err)
	}
	journal = durableJournalWithObjects(1, nil, nil)
	journal.CleanupIDs = append(journal.CleanupIDs, strings.Repeat("x", durableIDMaxBytes+1))
	if err := validateDurableJournal(journal); !errors.Is(err, errDurableJournalCorrupt) {
		t.Fatalf("oversized cleanup identifier passed validation: %v", err)
	}
}
