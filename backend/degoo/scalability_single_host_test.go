//go:build linux

package degoo

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// This test-only model uses a required, fsynced single-host registry outside
// the disposable cache and immutable transaction IDs. It does not establish
// that Degoo provides create-only IDs, authenticated direct reads, or exact-ID
// deletes. It also does not detect restoring an older valid registry copy.
const (
	shInventoryCap = 16
	shIntentCap    = 2
	shApplyNow     = iota
	shDelayAccept
	shNeverAccept
)

var (
	errSHUnknown  = errors.New("remote outcome remains unknown")
	errSHAck      = errors.New("simulated acknowledgement was lost")
	errSHIntent   = errors.New("unresolved intent cap reached")
	errSHSlots    = errors.New("inventory slot cap reached")
	errSHBusy     = errors.New("unresolved create blocks root publication")
	errSHLost     = errors.New("required safety registry is missing")
	errSHRetired  = errors.New("retired immutable ID cannot be referenced")
	errSHPoisoned = errors.New("checkpoint outcome is uncertain; reopen the registry")
)

type shObject struct {
	ID, Role, Hash string
	Gen, Slot      uint64
	Refs           []string
}

type shIntent struct {
	Op, Tx, ID, Role, Hash string
	Gen, Slot              uint64
	Refs                   []string
}

type shRegistry struct {
	Version, NextGen  int
	NextSlot          uint64
	Current, Previous string
	Objects           []shObject
	Intents           []shIntent
	Retired           []string
}

type shPayload struct {
	Tx, Role, DataHash string
	Gen                uint64
	Refs               []string
	Data               []byte
}

type shRemote struct {
	objects, pending              map[string][]byte
	deletePending                 []string
	deleteTargets                 []string
	creates, deletes, gets, lists int
	beforeCreate                  func(string) error
}

func newSHRemote() *shRemote {
	return &shRemote{objects: map[string][]byte{}, pending: map[string][]byte{}}
}

func (r *shRemote) create(id string, sealed []byte, mode int) error {
	r.creates++
	if r.beforeCreate != nil {
		if err := r.beforeCreate(id); err != nil {
			return err
		}
	}
	switch mode {
	case shNeverAccept:
		return errSHAck
	case shDelayAccept:
		r.pending[id] = append([]byte(nil), sealed...)
		return errSHAck
	}
	if _, exists := r.objects[id]; exists {
		return errors.New("immutable ID already exists")
	}
	r.objects[id] = append([]byte(nil), sealed...)
	return nil
}

func (r *shRemote) applyLateCreate(id string) error {
	sealed, ok := r.pending[id]
	if !ok {
		return errors.New("no late create for exact ID")
	}
	if _, exists := r.objects[id]; exists {
		return errors.New("late create would overwrite")
	}
	r.objects[id] = sealed
	delete(r.pending, id)
	return nil
}

func (r *shRemote) get(id string) ([]byte, bool) {
	r.gets++
	b, ok := r.objects[id]
	return append([]byte(nil), b...), ok
}

func (r *shRemote) deleteExact(id string, delayed bool) error {
	r.deletes++
	r.deleteTargets = append(r.deleteTargets, id)
	if delayed {
		r.deletePending = append(r.deletePending, id)
		return errSHAck
	}
	delete(r.objects, id)
	return nil
}

func (r *shRemote) applyLateDelete(id string) error {
	for i, target := range r.deletePending {
		if target == id {
			delete(r.objects, target)
			r.deletePending = append(r.deletePending[:i], r.deletePending[i+1:]...)
			return nil
		}
	}
	return errors.New("no late delete for exact ID")
}

type shStore struct {
	dir         string
	r           *shRemote
	key         []byte
	s           shRegistry
	poisoned    bool
	afterRename func() error
}

// newSHStore and openSHStore use the existing journal's fsync/rename/parent-
// fsync helper; the selected on-disk format remains test-only.
func newSHStore(dir string, r *shRemote) (*shStore, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	s := &shStore{dir: dir, r: r, key: shKey(), s: shRegistry{Version: 1, NextGen: 1}}
	return s, s.save(s.s)
}

func openSHStore(dir string, r *shRemote) (*shStore, error) {
	b, err := os.ReadFile(filepath.Join(dir, durableJournalFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil, errSHLost
	}
	if err != nil {
		return nil, err
	}
	var state shRegistry
	if len(b) > durableJournalMaxBytes || json.Unmarshal(b, &state) != nil || validateSH(state) != nil {
		return nil, errors.New("required safety registry is invalid")
	}
	return &shStore{dir: dir, r: r, key: shKey(), s: state}, nil
}

func (s *shStore) save(state shRegistry) error {
	if s.poisoned {
		return errSHPoisoned
	}
	if err := validateSH(state); err != nil {
		return err
	}
	b, err := json.Marshal(state)
	if err != nil {
		return err
	}
	if err = atomicDurableReplaceAfterRename(s.dir, durableJournalFile, b, s.afterRename); err != nil {
		s.poisoned = true
		return err
	}
	s.s = state
	return nil
}

func validateSH(s shRegistry) error {
	if s.Version != 1 || s.NextGen < 1 || s.NextSlot > shInventoryCap || len(s.Intents) > shIntentCap || len(s.Objects)+countSHCreates(s.Intents) > shInventoryCap {
		return errors.New("invalid bounded safety registry")
	}
	ids, slots, objects := map[string]bool{}, map[uint64]bool{}, map[string]shObject{}
	for _, o := range s.Objects {
		if o.ID == "" || o.Slot >= s.NextSlot || ids[o.ID] || slots[o.Slot] {
			return errors.New("duplicate registry allocation")
		}
		ids[o.ID], slots[o.Slot], objects[o.ID] = true, true, o
	}
	for _, in := range s.Intents {
		if in.ID == "" || in.Tx == "" {
			return errors.New("invalid registry intent")
		}
		switch in.Op {
		case "create":
			if in.Slot >= s.NextSlot || ids[in.ID] || slots[in.Slot] {
				return errors.New("duplicate create reservation")
			}
			ids[in.ID], slots[in.Slot] = true, true
		case "delete":
			o, ok := objects[in.ID]
			if !ok || o.Slot != in.Slot || !hasSH(s.Retired, in.ID) {
				return errors.New("invalid exact delete intent")
			}
		default:
			return errors.New("unknown registry intent")
		}
	}
	for _, id := range []string{s.Current, s.Previous} {
		if id != "" && objects[id].Role != "root" {
			return errors.New("invalid root locator")
		}
	}
	return nil
}

func countSHCreates(intents []shIntent) int {
	n := 0
	for _, in := range intents {
		if in.Op == "create" {
			n++
		}
	}
	return n
}

func (s *shStore) createData(tx string, data []byte, mode int) (string, error) {
	return s.create("data", tx, 0, nil, data, mode)
}

func (s *shStore) createRoot(tx, dataID string, mode int) (string, error) {
	if s.poisoned {
		return "", errSHPoisoned
	}
	if countSHCreates(s.s.Intents) != 0 {
		return "", errSHBusy
	}
	if hasSH(s.s.Retired, dataID) {
		return "", errSHRetired
	}
	if _, ok := findSH(s.s.Objects, dataID); !ok {
		return "", errors.New("root data reference is unknown")
	}
	return s.create("root", tx, uint64(s.s.NextGen), []string{dataID}, nil, mode)
}

func (s *shStore) create(role, tx string, gen uint64, refs []string, data []byte, mode int) (string, error) {
	if s.poisoned {
		return "", errSHPoisoned
	}
	if len(s.s.Intents) >= shIntentCap {
		return "", errSHIntent
	}
	if len(s.s.Objects)+countSHCreates(s.s.Intents) >= shInventoryCap || s.s.NextSlot >= shInventoryCap {
		return "", errSHSlots
	}
	for _, id := range refs {
		if hasSH(s.s.Retired, id) {
			return "", errSHRetired
		}
		if _, ok := findSH(s.s.Objects, id); !ok {
			return "", errors.New("unknown immutable reference")
		}
	}
	id := fmt.Sprintf("tx/%08d/%s/%s", gen, tx, role)
	dataHash := ""
	if role == "root" && len(refs) == 1 {
		child, ok := findSH(s.s.Objects, refs[0])
		if !ok || child.Role != "data" {
			return "", errors.New("root child is not a known data object")
		}
		dataHash = child.Hash
	}
	p := shPayload{Tx: tx, Role: role, Gen: gen, DataHash: dataHash, Refs: append([]string(nil), refs...), Data: append([]byte(nil), data...)}
	plain, err := json.Marshal(p)
	if err != nil {
		return "", err
	}
	sealed, err := shSeal(s.key, id, plain)
	if err != nil {
		return "", err
	}
	hash := shHash(plain)
	in := shIntent{Op: "create", Tx: tx, ID: id, Role: role, Gen: gen, Slot: s.s.NextSlot, Refs: refs, Hash: hash}
	next := s.s
	next.NextSlot++
	if role == "root" {
		next.NextGen++
	}
	next.Intents = append(append([]shIntent(nil), s.s.Intents...), in)
	if err := s.save(next); err != nil {
		return "", err
	}
	if err := s.r.create(id, sealed, mode); err != nil {
		return id, err
	}
	return id, s.reconcile(id)
}

func (s *shStore) reconcile(id string) error {
	if s.poisoned {
		return errSHPoisoned
	}
	i := -1
	var in shIntent
	for n, candidate := range s.s.Intents {
		if candidate.Op == "create" && candidate.ID == id {
			i, in = n, candidate
			break
		}
	}
	if i < 0 {
		return errors.New("create intent missing")
	}
	for _, ref := range in.Refs {
		if hasSH(s.s.Retired, ref) || hasSHDeleteIntent(s.s, ref) {
			return errSHRetired
		}
	}
	sealed, ok := s.r.get(id)
	if !ok {
		return errSHUnknown
	}
	plain, err := shOpen(s.key, id, sealed)
	if err != nil || shHash(plain) != in.Hash {
		return errors.New("remote object failed authentication")
	}
	var p shPayload
	if json.Unmarshal(plain, &p) != nil || p.Tx != in.Tx || p.Role != in.Role || p.Gen != in.Gen || !reflect.DeepEqual(p.Refs, in.Refs) {
		return errors.New("remote object did not match durable intent")
	}
	if in.Role == "root" {
		if len(p.Refs) != 1 {
			return errors.New("root has an invalid child reference count")
		}
		child, ok := findSH(s.s.Objects, p.Refs[0])
		if !ok || p.DataHash != child.Hash {
			return errors.New("root child digest does not match its allocation record")
		}
	}
	next := s.s
	next.Intents = append(append([]shIntent(nil), s.s.Intents[:i]...), s.s.Intents[i+1:]...)
	next.Objects = append(append([]shObject(nil), s.s.Objects...), shObject{ID: id, Role: in.Role, Hash: in.Hash, Gen: in.Gen, Slot: in.Slot, Refs: in.Refs})
	if in.Role == "root" {
		next.Previous, next.Current = next.Current, id
	}
	return s.save(next)
}

func (s *shStore) retireDelete(id string) error {
	if s.poisoned {
		return errSHPoisoned
	}
	if len(s.s.Intents) >= shIntentCap {
		return errSHIntent
	}
	if protectedSH(s.s, id) {
		return errors.New("current/previous root or unresolved create references delete target")
	}
	if hasSH(s.s.Retired, id) || hasSHDeleteIntent(s.s, id) {
		return errSHRetired
	}
	o, ok := findSH(s.s.Objects, id)
	if !ok {
		return errors.New("delete target is unknown")
	}
	next := s.s
	next.Retired = append(append([]string(nil), s.s.Retired...), id)
	next.Intents = append(append([]shIntent(nil), s.s.Intents...), shIntent{Op: "delete", Tx: "delete-" + id, ID: id, Slot: o.Slot})
	if err := s.save(next); err != nil {
		return err
	}
	return s.r.deleteExact(id, true)
}

func (s *shStore) readCurrent() ([]byte, error) {
	if s.poisoned {
		return nil, errSHPoisoned
	}
	root, ok := findSH(s.s.Objects, s.s.Current)
	if !ok || root.Role != "root" {
		return nil, errors.New("required root locator missing")
	}
	b, ok := s.r.get(root.ID)
	if !ok {
		return nil, errSHUnknown
	}
	plain, err := shOpen(s.key, root.ID, b)
	if err != nil || shHash(plain) != root.Hash {
		return nil, errors.New("root authentication failed")
	}
	var p shPayload
	if json.Unmarshal(plain, &p) != nil || p.Role != "root" || len(p.Refs) != 1 || p.DataHash == "" {
		return nil, errors.New("invalid encrypted root graph")
	}
	b, ok = s.r.get(p.Refs[0])
	if !ok {
		return nil, errSHUnknown
	}
	plain, err = shOpen(s.key, p.Refs[0], b)
	if err != nil || shHash(plain) != p.DataHash {
		return nil, errors.New("data authentication failed")
	}
	if json.Unmarshal(plain, &p) != nil || p.Role != "data" {
		return nil, errors.New("invalid encrypted data object")
	}
	return p.Data, nil
}

func findSH(objects []shObject, id string) (shObject, bool) {
	for _, o := range objects {
		if o.ID == id {
			return o, true
		}
	}
	return shObject{}, false
}

func hasSH(ids []string, id string) bool {
	for _, x := range ids {
		if x == id {
			return true
		}
	}
	return false
}

func protectedSH(s shRegistry, id string) bool {
	for _, rootID := range []string{s.Current, s.Previous} {
		root, ok := findSH(s.Objects, rootID)
		if ok && (root.ID == id || hasSH(root.Refs, id)) {
			return true
		}
	}
	for _, intent := range s.Intents {
		if intent.Op == "create" && hasSH(intent.Refs, id) {
			return true
		}
	}
	return false
}

func hasSHDeleteIntent(s shRegistry, id string) bool {
	for _, intent := range s.Intents {
		if intent.Op == "delete" && intent.ID == id {
			return true
		}
	}
	return false
}

func shKey() []byte { return []byte("0123456789abcdef0123456789abcdef") }

func shSeal(key []byte, id string, plain []byte) ([]byte, error) {
	b, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	a, err := cipher.NewGCM(b)
	if err != nil {
		return nil, err
	}
	n := make([]byte, a.NonceSize())
	if _, err := rand.Read(n); err != nil {
		return nil, err
	}
	return append(append([]byte(nil), n...), a.Seal(nil, n, plain, []byte(id))...), nil
}

func shOpen(key []byte, id string, sealed []byte) ([]byte, error) {
	b, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	a, err := cipher.NewGCM(b)
	if err != nil {
		return nil, err
	}
	if len(sealed) < a.NonceSize() {
		return nil, errors.New("truncated ciphertext")
	}
	return a.Open(nil, sealed[:a.NonceSize()], sealed[a.NonceSize():], []byte(id))
}

func shHash(b []byte) string { sum := sha256.Sum256(b); return hex.EncodeToString(sum[:]) }

func checkSH(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func dataSH(t *testing.T, s *shStore, tx, value string) string {
	t.Helper()
	id, err := s.createData(tx, []byte(value), shApplyNow)
	checkSH(t, err)
	return id
}

func rootSH(t *testing.T, s *shStore, tx, data string) string {
	t.Helper()
	id, err := s.createRoot(tx, data, shApplyNow)
	checkSH(t, err)
	return id
}

func seedSH(t *testing.T, dir string, r *shRemote) (*shStore, string) {
	t.Helper()
	s, err := newSHStore(dir, r)
	checkSH(t, err)
	id := dataSH(t, s, "payload", "remote encrypted content")
	rootSH(t, s, "root-one", id)
	return s, id
}

func TestScalabilitySingleHostLateCreateAndColdCache(t *testing.T) {
	dirs := []string{filepath.Join(t.TempDir(), "accepted"), filepath.Join(t.TempDir(), "never")}
	rs := []*shRemote{newSHRemote(), newSHRemote()}
	ss := make([]*shStore, 2)
	for i := range ss {
		ss[i], _ = seedSH(t, dirs[i], rs[i])
	}
	root1Backup, err := os.ReadFile(filepath.Join(dirs[0], durableJournalFile))
	checkSH(t, err)
	latest := make([]string, len(ss))
	for i := range ss {
		var err error
		latest[i], err = ss[i].createData("payload-two", []byte("latest remote content"), shApplyNow)
		checkSH(t, err)
		mode := shDelayAccept
		if i == 1 {
			mode = shNeverAccept
		}
		if _, err := ss[i].createRoot("root-two", latest[i], mode); !errors.Is(err, errSHAck) {
			t.Fatalf("trace %d lost-ack result: %v", i, err)
		}
	}
	b0, _ := os.ReadFile(filepath.Join(dirs[0], durableJournalFile))
	b1, _ := os.ReadFile(filepath.Join(dirs[1], durableJournalFile))
	if !bytes.Equal(b0, b1) {
		t.Fatal("late-accepted and never-accepted traces differ before apply")
	}
	in := ss[0].s.Intents[len(ss[0].s.Intents)-1]
	calls := []int{rs[0].creates, rs[1].creates}
	for i := range ss {
		ss[i], _ = openSHStore(dirs[i], rs[i])
		if err := ss[i].reconcile(in.ID); !errors.Is(err, errSHUnknown) {
			t.Fatalf("trace %d absence treated as terminal: %v", i, err)
		}
		if _, err := ss[i].createRoot("root-three", ss[i].s.Objects[0].ID, shApplyNow); !errors.Is(err, errSHBusy) {
			t.Fatalf("trace %d published with unknown root: %v", i, err)
		}
		if rs[i].creates != calls[i] {
			t.Fatalf("trace %d replayed request", i)
		}
	}
	b0, _ = os.ReadFile(filepath.Join(dirs[0], durableJournalFile))
	b1, _ = os.ReadFile(filepath.Join(dirs[1], durableJournalFile))
	if !bytes.Equal(b0, b1) {
		t.Fatal("finite absent reads distinguished the traces")
	}
	if err := rs[0].applyLateCreate(in.ID); err != nil {
		t.Fatal(err)
	}
	if err := ss[0].reconcile(in.ID); err != nil {
		t.Fatalf("positive exact authenticated read: %v", err)
	}
	current, _ := findSH(ss[0].s.Objects, ss[0].s.Current)
	previous, _ := findSH(ss[0].s.Objects, ss[0].s.Previous)
	if current.Gen != 2 || previous.Gen != 1 || current.ID == previous.ID || ss[0].s.NextGen != 3 {
		t.Fatalf("bad generation/root history: %+v %+v", current, previous)
	}
	cache := filepath.Join(dirs[0], "cache")
	checkSH(t, os.Mkdir(cache, 0700))
	checkSH(t, os.WriteFile(filepath.Join(cache, "derived"), []byte("throwaway"), 0600))
	checkSH(t, os.RemoveAll(cache))
	gets, lists := rs[0].gets, rs[0].lists
	root, ok := findSH(ss[0].s.Objects, ss[0].s.Current)
	if !ok {
		t.Fatal("current root is absent from safety registry")
	}
	view := *ss[0]
	root.Refs = nil // The required locator has no child ID or child digest.
	view.s.Objects = []shObject{root}
	view.s.Previous = ""
	content, err := view.readCurrent()
	if err != nil || string(content) != "latest remote content" {
		t.Fatalf("cold-cache rebuild: %q, %v", content, err)
	}
	if rs[0].gets <= gets || rs[0].lists != lists {
		t.Fatal("cold rebuild did not fetch by exact locator without listing")
	}
	checkSH(t, os.WriteFile(filepath.Join(dirs[0], durableJournalFile), root1Backup, 0600))
	stale, err := openSHStore(dirs[0], rs[0])
	checkSH(t, err)
	if stale.s.Current == current.ID {
		t.Fatal("stale valid registry unexpectedly points at the latest root")
	}
	oldContent, err := stale.readCurrent()
	if err != nil || string(oldContent) != "remote encrypted content" {
		t.Fatalf("stale valid registry was not accepted as an older view: %q, %v", oldContent, err)
	}
	gets = rs[0].gets
	checkSH(t, os.Remove(filepath.Join(dirs[0], durableJournalFile)))
	if _, err := openSHStore(dirs[0], rs[0]); !errors.Is(err, errSHLost) {
		t.Fatalf("registry loss did not refuse: %v", err)
	}
	if rs[0].gets != gets || rs[0].lists != lists {
		t.Fatal("registry loss attempted remote-only bootstrap")
	}
}

func TestScalabilitySingleHostRetireDeleteProtectsUnresolvedRootChild(t *testing.T) {
	dir, remote := t.TempDir(), newSHRemote()
	store, _ := seedSH(t, dir, remote)
	current, previous := store.s.Current, store.s.Previous
	child := dataSH(t, store, "candidate-data", "payload needed by candidate root")
	root, err := store.createRoot("candidate-root", child, shDelayAccept)
	if !errors.Is(err, errSHAck) {
		t.Fatalf("candidate root did not remain unresolved: id=%q err=%v", root, err)
	}

	store, err = openSHStore(dir, remote)
	checkSH(t, err)
	if len(store.s.Intents) != 1 || store.s.Intents[0].ID != root || !hasSH(store.s.Intents[0].Refs, child) {
		t.Fatalf("reopen lost the candidate root's child reference: %+v", store.s.Intents)
	}
	deleteCalls := remote.deletes
	if err := store.retireDelete(child); err == nil {
		t.Fatal("retiring a child of an unresolved root was not refused")
	}
	if len(store.s.Intents) != 1 || store.s.Intents[0].Op != "create" || store.s.Intents[0].ID != root || !hasSH(store.s.Intents[0].Refs, child) {
		t.Fatalf("refused retirement did not retain the root intent: %+v", store.s.Intents)
	}
	if remote.deletes != deleteCalls || len(remote.deletePending) != 0 || hasSH(store.s.Retired, child) {
		t.Fatalf("refused retirement scheduled a delete: calls=%d pending=%v retired=%v", remote.deletes, remote.deletePending, store.s.Retired)
	}
	if store.s.Current != current || store.s.Previous != previous {
		t.Fatalf("refused retirement changed root history: current=%q previous=%q", store.s.Current, store.s.Previous)
	}

	if err := remote.applyLateCreate(root); err != nil {
		t.Fatal(err)
	}
	if err := store.reconcile(root); err != nil {
		t.Fatalf("candidate root did not reconcile after protected child remained available: %v", err)
	}
	content, err := store.readCurrent()
	if err != nil || string(content) != "payload needed by candidate root" {
		t.Fatalf("reconciled root lost its child: content=%q err=%v", content, err)
	}
}

func TestScalabilitySingleHostReconcileRefusesRetiredChildCandidates(t *testing.T) {
	tests := []struct {
		name       string
		deleteMode int // 0 means retired only, 1 means deletion is unresolved, 2 means it applied
	}{
		{name: "retired"},
		{name: "delete unresolved", deleteMode: 1},
		{name: "delete applied", deleteMode: 2},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir, remote := t.TempDir(), newSHRemote()
			store, _ := seedSH(t, dir, remote)
			current, previous := store.s.Current, store.s.Previous
			child := dataSH(t, store, "candidate-data", "candidate payload")
			root, err := store.createRoot("candidate-root", child, shDelayAccept)
			if !errors.Is(err, errSHAck) {
				t.Fatalf("candidate root did not remain unresolved: id=%q err=%v", root, err)
			}
			childRecord, ok := findSH(store.s.Objects, child)
			if !ok {
				t.Fatalf("candidate child %q is absent from the safety registry", child)
			}

			// Persist overlapping intents to verify recovery refuses a candidate
			// whose child is already retired.
			unsafe := store.s
			unsafe.Retired = append(append([]string(nil), store.s.Retired...), child)
			if test.deleteMode != 0 {
				unsafe.Intents = append(append([]shIntent(nil), store.s.Intents...), shIntent{
					Op: "delete", Tx: "delete-" + child, ID: child, Slot: childRecord.Slot,
				})
			}
			checkSH(t, store.save(unsafe))
			store, err = openSHStore(dir, remote)
			checkSH(t, err)
			if test.deleteMode != 0 {
				delayed := test.deleteMode == 1
				err := remote.deleteExact(child, delayed)
				if delayed && !errors.Is(err, errSHAck) || !delayed && err != nil {
					t.Fatalf("could not install legacy delete outcome: delayed=%v err=%v", delayed, err)
				}
			}
			if err := remote.applyLateCreate(root); err != nil {
				t.Fatal(err)
			}

			before, err := json.Marshal(store.s)
			checkSH(t, err)
			err = store.reconcile(root)
			if !errors.Is(err, errSHRetired) {
				t.Errorf("reconcile accepted candidate root with retired/deleting child: %v", err)
			}
			after, marshalErr := json.Marshal(store.s)
			checkSH(t, marshalErr)
			if !bytes.Equal(before, after) {
				t.Errorf("refused candidate reconciliation mutated the safety registry")
			}
			if store.s.Current != current || store.s.Previous != previous {
				t.Errorf("candidate reconciliation changed root history: current=%q previous=%q", store.s.Current, store.s.Previous)
			}
			if len(store.s.Intents) == 0 || store.s.Intents[0].ID != root || !hasSH(store.s.Intents[0].Refs, child) {
				t.Errorf("candidate reconciliation discarded its unresolved root intent: %+v", store.s.Intents)
			}

			if test.deleteMode == 1 {
				if err := remote.applyLateDelete(child); err != nil {
					t.Fatal(err)
				}
			}
			content, readErr := store.readCurrent()
			if readErr != nil || string(content) != "remote encrypted content" {
				t.Errorf("candidate reconciliation damaged the prior current root: content=%q err=%v", content, readErr)
			}
		})
	}
}

func TestScalabilitySingleHostLateDeleteUsesExactID(t *testing.T) {
	dir, r := t.TempDir(), newSHRemote()
	s, err := newSHStore(dir, r)
	checkSH(t, err)
	old, err := s.createData("staged-old", []byte("old"), shApplyNow)
	checkSH(t, err)
	newID, err := s.createData("live", []byte("new"), shApplyNow)
	checkSH(t, err)
	for n := 1; n <= 4; n++ {
		rootSH(t, s, fmt.Sprintf("root-%d", n), newID)
	}
	if protectedSH(s.s, old) {
		t.Fatal("old object is still current/previous")
	}
	oldRecord, _ := findSH(s.s.Objects, old)
	if err := s.retireDelete(old); !errors.Is(err, errSHAck) {
		t.Fatalf("late delete: %v", err)
	}
	s, err = openSHStore(dir, r)
	checkSH(t, err)
	root5, err := s.createRoot("root-five", newID, shApplyNow)
	if err != nil {
		t.Fatalf("independent root blocked by exact old ID delete: %v", err)
	}
	if err := r.applyLateDelete(old); err != nil {
		t.Fatal(err)
	}
	if len(r.deleteTargets) != 1 || r.deleteTargets[0] != old {
		t.Fatalf("delete target changed: %v", r.deleteTargets)
	}
	for _, id := range []string{newID, root5, s.s.Current, s.s.Previous} {
		if _, ok := r.objects[id]; !ok {
			t.Fatalf("late delete of %q removed newer object %q", old, id)
		}
	}
	if _, ok := r.objects[old]; ok || !hasSH(s.s.Retired, old) {
		t.Fatal("old ID was not retired/deleted")
	}
	if len(s.s.Intents) != 1 || s.s.Intents[0].Op != "delete" {
		t.Fatal("absence incorrectly cleared delayed-delete uncertainty")
	}
	newRecord, _ := findSH(s.s.Objects, root5)
	if newRecord.Slot <= oldRecord.Slot || newRecord.ID == oldRecord.ID || s.s.NextSlot > shInventoryCap {
		t.Fatal("late delete could alias a reused slot/ID")
	}
	if _, err := s.createRoot("bad-reference", old, shApplyNow); !errors.Is(err, errSHRetired) {
		t.Fatalf("retired object reused: %v", err)
	}
}

func TestScalabilitySingleHostUnknownIntentAndInventoryCaps(t *testing.T) {
	dir, r := t.TempDir(), newSHRemote()
	s, err := newSHStore(dir, r)
	checkSH(t, err)
	for i, mode := range []int{shDelayAccept, shNeverAccept} {
		if _, err := s.createData(fmt.Sprintf("unknown-%d", i), []byte("x"), mode); !errors.Is(err, errSHAck) {
			t.Fatalf("unknown create: %v", err)
		}
	}
	calls := r.creates
	if _, err := s.createData("over-cap", []byte("x"), shApplyNow); !errors.Is(err, errSHIntent) {
		t.Fatalf("intent cap: %v", err)
	}
	if r.creates != calls || len(s.s.Intents) != shIntentCap || s.s.NextSlot != shIntentCap {
		t.Fatal("intent cap was not enforced before a third request")
	}
	info, err := os.Stat(filepath.Join(dir, durableJournalFile))
	checkSH(t, err)
	if info.Mode().Perm() != 0600 {
		t.Fatalf("registry mode %o, want 600", info.Mode().Perm())
	}

	full, err := newSHStore(t.TempDir(), newSHRemote())
	checkSH(t, err)
	for i := 0; i < shInventoryCap; i++ {
		dataSH(t, full, fmt.Sprintf("slot-%d", i), "x")
	}
	calls = full.r.creates
	if _, err := full.createData("slot-overflow", []byte("x"), shApplyNow); !errors.Is(err, errSHSlots) {
		t.Fatalf("inventory cap: %v", err)
	}
	if full.r.creates != calls || full.s.NextSlot != shInventoryCap || len(full.r.objects) > shInventoryCap {
		t.Fatal("inventory bound failed")
	}
}

func TestScalabilitySingleHostPostRenameFailurePoisonsUntilReopen(t *testing.T) {
	dir, r := t.TempDir(), newSHRemote()
	s, err := newSHStore(dir, r)
	checkSH(t, err)
	checkpointFailure := errors.New("injected directory-sync failure after rename")
	s.afterRename = func() error { return checkpointFailure }
	if _, err := s.createData("ambiguous", []byte("x"), shApplyNow); !errors.Is(err, checkpointFailure) {
		t.Fatalf("post-rename checkpoint failure: %v", err)
	}
	id := "tx/00000000/ambiguous/data"
	if !s.poisoned || r.creates != 0 {
		t.Fatalf("failed checkpoint did not poison before remote create: poisoned=%v calls=%d", s.poisoned, r.creates)
	}
	if _, err := s.createData("another", []byte("x"), shApplyNow); !errors.Is(err, errSHPoisoned) {
		t.Fatalf("create after uncertain checkpoint: %v", err)
	}
	if _, err := s.createRoot("another-root", "", shApplyNow); !errors.Is(err, errSHPoisoned) {
		t.Fatalf("root publication after uncertain checkpoint: %v", err)
	}
	if err := s.reconcile(id); !errors.Is(err, errSHPoisoned) {
		t.Fatalf("reconcile after uncertain checkpoint: %v", err)
	}
	if err := s.retireDelete(id); !errors.Is(err, errSHPoisoned) {
		t.Fatalf("delete after uncertain checkpoint: %v", err)
	}
	if err := s.save(s.s); !errors.Is(err, errSHPoisoned) {
		t.Fatalf("save after uncertain checkpoint: %v", err)
	}
	if _, err := s.readCurrent(); !errors.Is(err, errSHPoisoned) {
		t.Fatalf("read from stale in-memory state: %v", err)
	}
	if r.creates != 0 || r.gets != 0 || r.deletes != 0 {
		t.Fatal("poisoned process issued a remote request")
	}

	reopened, err := openSHStore(dir, r)
	checkSH(t, err)
	if reopened.s.NextSlot != 1 || len(reopened.s.Intents) != 1 || reopened.s.Intents[0].ID != id {
		t.Fatalf("reopen did not load the renamed durable intent: %+v", reopened.s)
	}
	if err := reopened.reconcile(id); !errors.Is(err, errSHUnknown) {
		t.Fatalf("reopened absent intent was not retained as unknown: %v", err)
	}
	if r.creates != 0 || r.gets != 1 {
		t.Fatalf("reopen replayed create instead of exact-read reconciliation: creates=%d gets=%d", r.creates, r.gets)
	}
}
