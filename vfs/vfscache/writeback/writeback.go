// Package writeback keeps track of the files which need to be written
// back to storage
package writeback

import (
	"container/heap"
	"context"
	"errors"
	"path"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/fserrors"
	"github.com/rclone/rclone/vfs/vfscommon"
)

const (
	maxUploadDelay = 5 * time.Minute // max exponential delay between upload attempts
)

// PutFn is the interface that item provides to store the data
type PutFn func(context.Context) error

// Handle is returned for callers to keep track of writeback items
type Handle uint64

// WriteBack keeps track of the items which need to be written back to the disk at some point
type WriteBack struct {
	// read and written with atomic, must be 64-bit aligned
	id Handle // id of the last writeBackItem created

	ctx         context.Context
	mu          sync.Mutex
	items       writeBackItems            // priority queue of *writeBackItem - writeBackItems are in here while awaiting transfer only
	lookup      map[Handle]*writeBackItem // for getting a *writeBackItem from a Handle - writeBackItems are in here until cancelled
	pauses      map[uint64]*prefixPauseState
	lastPauseID uint64
	opt         *vfscommon.Options // VFS options
	timer       *time.Timer        // next scheduled time for the uploader
	expiry      time.Time          // time the next item expires or IsZero
	uploads     int                // number of uploads in progress
}

// PrefixPause holds queued writebacks in a path subtree until it is released.
type PrefixPause struct {
	wb       *WriteBack
	id       uint64
	once     sync.Once
	released chan struct{}
}

type prefixPauseState struct {
	prefix string
	items  map[*writeBackItem]struct{}
}

// New make a new WriteBack
//
// cancel the context to stop the background processing
func New(ctx context.Context, opt *vfscommon.Options) *WriteBack {
	wb := &WriteBack{
		ctx:    ctx,
		items:  writeBackItems{},
		lookup: make(map[Handle]*writeBackItem),
		pauses: make(map[uint64]*prefixPauseState),
		opt:    opt,
	}
	heap.Init(&wb.items)
	return wb
}

// writeBackItem stores an Item awaiting writeback
//
// These are stored on the items heap when awaiting transfer but
// removed from the items heap when transferring. They remain in the
// lookup map until cancelled.
//
// writeBack.mu must be held to manipulate this
type writeBackItem struct {
	name      string             // name of the item so we don't have to read it from item
	size      int64              // size of the item so we don't have to read it from item
	id        Handle             // id of the item
	index     int                // index into the priority queue for update
	expiry    time.Time          // When this expires we will write it back
	uploading bool               // True if item is being processed by upload() method
	onHeap    bool               // true if this item is on the items heap
	cancel    context.CancelFunc // To cancel the upload with
	done      chan struct{}      // closed when the cancellation completes
	putFn     PutFn              // To write the object data
	tries     int                // number of times we have tried to upload
	delay     time.Duration      // ordinary exponential delay between upload attempts
}

// A writeBackItems implements a priority queue by implementing
// heap.Interface and holds writeBackItems.
type writeBackItems []*writeBackItem

func (ws writeBackItems) Len() int { return len(ws) }

func (ws writeBackItems) Less(i, j int) bool {
	a, b := ws[i], ws[j]
	// If times are equal then use ID to disambiguate
	if a.expiry.Equal(b.expiry) {
		return a.id < b.id
	}
	return a.expiry.Before(b.expiry)
}

func (ws writeBackItems) Swap(i, j int) {
	ws[i], ws[j] = ws[j], ws[i]
	ws[i].index = i
	ws[j].index = j
}

func (ws *writeBackItems) Push(x any) {
	n := len(*ws)
	item := x.(*writeBackItem)
	item.index = n
	*ws = append(*ws, item)
}

func (ws *writeBackItems) Pop() any {
	old := *ws
	n := len(old)
	item := old[n-1]
	old[n-1] = nil  // avoid memory leak
	item.index = -1 // for safety
	*ws = old[0 : n-1]
	return item
}

// update modifies the expiry of an Item in the queue.
//
// call with lock held
func (ws *writeBackItems) _update(item *writeBackItem, expiry time.Time) {
	item.expiry = expiry
	heap.Fix(ws, item.index)
}

// return a new expiry time based from now until the WriteBack timeout
//
// call with lock held
func (wb *WriteBack) _newExpiry() time.Time {
	expiry := time.Now()
	if wb.opt.WriteBack > 0 {
		expiry = expiry.Add(time.Duration(wb.opt.WriteBack))
	}
	// expiry = expiry.Round(time.Millisecond)
	return expiry
}

// make a new writeBackItem
//
// call with the lock held
func (wb *WriteBack) _newItem(id Handle, name string, size int64) *writeBackItem {
	wb.SetID(&id)
	wbItem := &writeBackItem{
		name:   name,
		size:   size,
		expiry: wb._newExpiry(),
		delay:  time.Duration(wb.opt.WriteBack),
		id:     id,
	}
	wb._addItem(wbItem)
	wb._pushItem(wbItem)
	return wbItem
}

// add a writeBackItem to the lookup map
//
// call with the lock held
func (wb *WriteBack) _addItem(wbItem *writeBackItem) {
	wb.lookup[wbItem.id] = wbItem
}

// delete a writeBackItem from the lookup map
//
// call with the lock held
func (wb *WriteBack) _delItem(wbItem *writeBackItem) {
	delete(wb.lookup, wbItem.id)
}

// pop a writeBackItem from the items heap
//
// call with the lock held
func (wb *WriteBack) _popItem() (wbItem *writeBackItem) {
	wbItem = heap.Pop(&wb.items).(*writeBackItem)
	wbItem.onHeap = false
	return wbItem
}

// push a writeBackItem onto the items heap
//
// call with the lock held
func (wb *WriteBack) _pushItem(wbItem *writeBackItem) {
	if !wbItem.onHeap {
		heap.Push(&wb.items, wbItem)
		wbItem.onHeap = true
	}
}

// remove a writeBackItem from the items heap
//
// call with the lock held
func (wb *WriteBack) _removeItem(wbItem *writeBackItem) {
	if wbItem.onHeap {
		heap.Remove(&wb.items, wbItem.index)
		wbItem.onHeap = false
	}
}

// peek the oldest writeBackItem - may be nil
//
// call with the lock held
func (wb *WriteBack) _peekItem() (wbItem *writeBackItem) {
	if len(wb.items) == 0 {
		return nil
	}
	return wb.items[0]
}

// _inPrefix reports whether name is prefix or a descendant of prefix.
func _inPrefix(name, prefix string) bool {
	name = path.Clean(name)
	if name == "." {
		name = ""
	}
	if prefix == "" || prefix == "." {
		return true
	}
	prefix = path.Clean(prefix)
	return name == prefix || strings.HasPrefix(name, prefix+"/")
}

// _itemInPause reports whether item belongs to state, including items which
// have been renamed out of the paused subtree while the pause is held.
func (wb *WriteBack) _itemInPause(state *prefixPauseState, item *writeBackItem) bool {
	if _, ok := state.items[item]; ok {
		return true
	}
	return _inPrefix(item.name, state.prefix)
}

// _bindItemToPauses records path membership before a queued item is renamed.
// Call with the lock held.
func (wb *WriteBack) _bindItemToPauses(item *writeBackItem, names ...string) {
	for _, state := range wb.pauses {
		if _, ok := state.items[item]; ok {
			continue
		}
		for _, name := range names {
			if _inPrefix(name, state.prefix) {
				state.items[item] = struct{}{}
				break
			}
		}
	}
}

// _peekUnpausedItem finds the earliest queued item which is not held by a
// prefix pause. Call with the lock held.
func (wb *WriteBack) _peekUnpausedItem() (next *writeBackItem) {
	if len(wb.pauses) == 0 {
		return wb._peekItem()
	}
	if len(wb.items) == 0 {
		return nil
	}
	if !wb._isPaused(wb.items[0]) {
		return wb.items[0]
	}
	for _, item := range wb.items {
		if !wb._isPaused(item) && (next == nil || wb.items.Less(item.index, next.index)) {
			next = item
		}
	}
	return next
}

// _isPaused reports whether a queued item is held by any prefix pause. Call
// with the lock held.
func (wb *WriteBack) _isPaused(item *writeBackItem) bool {
	for _, state := range wb.pauses {
		if wb._itemInPause(state, item) {
			return true
		}
	}
	return false
}

// PausePrefix holds new queued uploads in prefix and waits for active uploads
// in that subtree to finish. It does not cancel uploads already in progress.
// The returned pause must be released when the operation protecting the
// subtree is complete. Context cancellation releases the pause automatically.
func (wb *WriteBack) PausePrefix(ctx context.Context, prefix string) (*PrefixPause, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	prefix = path.Clean(prefix)
	if prefix == "." {
		prefix = ""
	}

	wb.mu.Lock()
	wb.lastPauseID++
	id := wb.lastPauseID
	state := &prefixPauseState{prefix: prefix, items: make(map[*writeBackItem]struct{})}
	for _, item := range wb.lookup {
		if _inPrefix(item.name, prefix) {
			state.items[item] = struct{}{}
		}
	}
	wb.pauses[id] = state
	wb._resetTimer()
	wb.mu.Unlock()

	pause := &PrefixPause{wb: wb, id: id, released: make(chan struct{})}
	go func() {
		select {
		case <-ctx.Done():
			pause.Release()
		case <-pause.released:
		}
	}()

	for {
		if err := ctx.Err(); err != nil {
			pause.Release()
			return nil, err
		}
		wb.mu.Lock()
		var done <-chan struct{}
		for _, item := range wb.lookup {
			if item.uploading && wb._itemInPause(state, item) {
				done = item.done
				break
			}
		}
		wb.mu.Unlock()
		if done == nil {
			if err := ctx.Err(); err != nil {
				pause.Release()
				return nil, err
			}
			return pause, nil
		}
		select {
		case <-done:
		case <-ctx.Done():
			pause.Release()
			return nil, ctx.Err()
		}
	}
}

// Release resumes queued uploads held by this prefix pause.
func (pause *PrefixPause) Release() {
	if pause == nil {
		return
	}
	pause.once.Do(func() {
		close(pause.released)
		pause.wb.mu.Lock()
		delete(pause.wb.pauses, pause.id)
		pause.wb._resetTimer()
		pause.wb.mu.Unlock()
	})
}

// stop the timer which runs the expiries
func (wb *WriteBack) _stopTimer() {
	if wb.expiry.IsZero() {
		return
	}
	wb.expiry = time.Time{}
	// fs.Debugf(nil, "resetTimer STOP")
	if wb.timer != nil {
		wb.timer.Stop()
		wb.timer = nil
	}
}

// reset the timer which runs the expiries
func (wb *WriteBack) _resetTimer() {
	wbItem := wb._peekUnpausedItem()
	if wbItem == nil {
		wb._stopTimer()
	} else {
		if wb.expiry.Equal(wbItem.expiry) {
			return
		}
		wb.expiry = wbItem.expiry
		dt := max(time.Until(wbItem.expiry), 0)
		// fs.Debugf(nil, "resetTimer dt=%v", dt)
		if wb.timer != nil {
			wb.timer.Stop()
		}
		wb.timer = time.AfterFunc(dt, func() {
			wb.processItems(wb.ctx)
		})
	}
}

// SetID sets the Handle pointed to if it is non zero to the next
// handle.
func (wb *WriteBack) SetID(pid *Handle) {
	if *pid == 0 {
		*pid = Handle(atomic.AddUint64((*uint64)(&wb.id), 1))
	}
}

// Add adds an item to the writeback queue or resets its timer if it
// is already there.
//
// If id is 0 then a new item will always be created and the new
// Handle will be returned.
//
// Use SetID to create Handles in advance of calling Add.
//
// If modified is false then it doesn't cancel a pending upload if
// there is one as there is no need.
func (wb *WriteBack) Add(id Handle, name string, size int64, modified bool, putFn PutFn) Handle {
	wb.mu.Lock()
	defer wb.mu.Unlock()

	wbItem, ok := wb.lookup[id]
	if !ok {
		wbItem = wb._newItem(id, name, size)
	} else {
		if wbItem.uploading && modified {
			// We are uploading already so cancel the upload
			wb._cancelUpload(wbItem)
		}
		// Kick the timer on
		wb.items._update(wbItem, wb._newExpiry())
	}
	wb._bindItemToPauses(wbItem, name, wbItem.name)
	wbItem.putFn = putFn
	wbItem.size = size
	wb._resetTimer()
	return wbItem.id
}

// _remove should be called when a file should be removed from the
// writeback queue. This cancels a writeback if there is one and
// doesn't return the item to the queue.
//
// This should be called with the lock held
func (wb *WriteBack) _remove(id Handle) (found bool) {
	wbItem, found := wb.lookup[id]
	if found {
		fs.Debugf(wbItem.name, "vfs cache: cancelling writeback (uploading %v) %p item %d", wbItem.uploading, wbItem, wbItem.id)
		if wbItem.uploading {
			// We are uploading already so cancel the upload
			wb._cancelUpload(wbItem)
		}
		// Remove the item from the heap
		wb._removeItem(wbItem)
		// Remove the item from the lookup map
		wb._delItem(wbItem)
	}
	wb._resetTimer()
	return found
}

// Remove should be called when a file should be removed from the
// writeback queue. This cancels a writeback if there is one and
// doesn't return the item to the queue.
func (wb *WriteBack) Remove(id Handle) (found bool) {
	wb.mu.Lock()
	defer wb.mu.Unlock()

	return wb._remove(id)
}

// Rename should be called when a file might be uploading and it gains
// a new name. This will cancel the upload and put it back in the
// queue.
func (wb *WriteBack) Rename(id Handle, name string) {
	wb.mu.Lock()
	defer wb.mu.Unlock()

	wbItem, ok := wb.lookup[id]
	if !ok {
		return
	}
	if wbItem.uploading {
		// We are uploading already so cancel the upload
		wb._cancelUpload(wbItem)
	}

	// Check to see if there are any uploads with the existing
	// name and remove them
	for existingID, existingItem := range wb.lookup {
		if existingID != id && existingItem.name == name {
			wb._remove(existingID)
		}
	}

	wb._bindItemToPauses(wbItem, wbItem.name, name)
	wbItem.name = name
	// Kick the timer on
	wb.items._update(wbItem, wb._newExpiry())

	wb._resetTimer()
}

// upload the item - called as a goroutine
//
// uploading will have been incremented here already
func (wb *WriteBack) upload(ctx context.Context, wbItem *writeBackItem) {
	wb.mu.Lock()
	defer wb.mu.Unlock()
	putFn := wbItem.putFn
	wbItem.tries++

	fs.Debugf(wbItem.name, "vfs cache: starting upload")

	wb.mu.Unlock()
	// Recover around the upload itself rather than the whole goroutine, so a
	// panicking backend is retried like any other upload failure below.
	err := vfscommon.RecoverCall(wbItem.name, func() error { return putFn(ctx) })
	wb.mu.Lock()

	wbItem.cancel() // cancel context to release resources since store done

	wbItem.uploading = false
	wb.uploads--

	if err != nil {
		// FIXME should this have a max number of transfer attempts?
		wbItem.delay *= 2
		if wbItem.delay > maxUploadDelay {
			wbItem.delay = maxUploadDelay
		}
		retryAt := time.Now().Add(wbItem.delay)
		if errors.Is(err, context.Canceled) {
			fs.Infof(wbItem.name, "vfs cache: upload canceled")
			// Upload was cancelled so reset timer
			wbItem.delay = time.Duration(wb.opt.WriteBack)
			retryAt = time.Now().Add(wbItem.delay)
		} else {
			if serverRetryAt := fserrors.RetryAfterErrorTime(err); serverRetryAt.After(retryAt) {
				retryAt = serverRetryAt
			}
			fs.Errorf(wbItem.name, "vfs cache: failed to upload try #%d, will retry in %v: %v", wbItem.tries, time.Until(retryAt), err)
		}
		// push the item back on the queue for retry
		wb._pushItem(wbItem)
		wb.items._update(wbItem, retryAt)
	} else {
		fs.Infof(wbItem.name, "vfs cache: upload succeeded try #%d", wbItem.tries)
		// show that we are done with the item
		wb._delItem(wbItem)
	}
	wb._resetTimer()
	close(wbItem.done)
}

// cancel the upload - the item should be on the heap after this returns
//
// call with lock held
func (wb *WriteBack) _cancelUpload(wbItem *writeBackItem) {
	if !wbItem.uploading {
		return
	}
	fs.Debugf(wbItem.name, "vfs cache: cancelling upload")
	if wbItem.cancel != nil {
		// Cancel the upload - this may or may not be effective
		wbItem.cancel()
		// wait for the uploader to finish
		//
		// we need to wait without the lock otherwise the
		// background part will never run.
		wb.mu.Unlock()
		<-wbItem.done
		wb.mu.Lock()
	}
	// uploading items are not on the heap so add them back
	wb._pushItem(wbItem)
	fs.Debugf(wbItem.name, "vfs cache: cancelled upload")
}

// cancelUpload cancels the upload of the item if there is one in progress
//
// it returns true if there was an upload in progress
func (wb *WriteBack) cancelUpload(id Handle) bool {
	wb.mu.Lock()
	defer wb.mu.Unlock()
	wbItem, ok := wb.lookup[id]
	if !ok || !wbItem.uploading {
		return false
	}
	wb._cancelUpload(wbItem)
	return true
}

// this uploads as many items as possible
func (wb *WriteBack) processItems(ctx context.Context) {
	wb.mu.Lock()
	defer wb.mu.Unlock()

	if wb.ctx.Err() != nil {
		return
	}

	resetTimer := true
	for wbItem := wb._peekUnpausedItem(); wbItem != nil; wbItem = wb._peekUnpausedItem() {
		// If reached transfer limit stop the timer - it will be
		// restarted when an upload finishes
		if wb.uploads >= fs.GetConfig(wb.ctx).Transfers {
			fs.Debugf(wbItem.name, "vfs cache: delaying writeback as --transfers exceeded")
			resetTimer = false
			break
		}
		// Stop if the next item hasn't expired yet
		if time.Until(wbItem.expiry) > 0 {
			break
		}
		// Pop the item, mark as uploading and start the uploader
		wb._removeItem(wbItem)
		//fs.Debugf(wbItem.name, "uploading = true %p item %p", wbItem, wbItem.item)
		wbItem.uploading = true
		wb.uploads++
		newCtx, cancel := context.WithCancel(ctx)
		wbItem.cancel = cancel
		wbItem.done = make(chan struct{})
		go wb.upload(newCtx, wbItem)
	}

	if resetTimer {
		wb._resetTimer()
	} else {
		wb._stopTimer()
	}
}

// Stats return the number of uploads in progress and queued
func (wb *WriteBack) Stats() (uploadsInProgress, uploadsQueued int) {
	wb.mu.Lock()
	defer wb.mu.Unlock()
	return wb.uploads, len(wb.items)
}

// QueueInfo is information about an item queued for upload, returned
// by Queue
type QueueInfo struct {
	Name      string  `json:"name"`      // name (full path) of the file,
	ID        Handle  `json:"id"`        // id of queue item
	Size      int64   `json:"size"`      // integer size of the file in bytes
	Expiry    float64 `json:"expiry"`    // seconds from now which the file is eligible for transfer, oldest goes first
	Tries     int     `json:"tries"`     // number of times we have tried to upload
	Delay     float64 `json:"delay"`     // ordinary exponential delay between upload attempts (s)
	Uploading bool    `json:"uploading"` // true if item is being uploaded
}

// Queue return info about the current upload queue
func (wb *WriteBack) Queue() []QueueInfo {
	wb.mu.Lock()
	defer wb.mu.Unlock()

	items := make([]QueueInfo, 0, len(wb.lookup))
	now := time.Now()

	// Lookup all the items in no particular order
	for _, wbItem := range wb.lookup {
		items = append(items, QueueInfo{
			Name:      wbItem.name,
			ID:        wbItem.id,
			Size:      wbItem.size,
			Expiry:    wbItem.expiry.Sub(now).Seconds(),
			Tries:     wbItem.tries,
			Delay:     wbItem.delay.Seconds(),
			Uploading: wbItem.uploading,
		})
	}

	// Sort by Uploading first then Expiry
	sort.Slice(items, func(i, j int) bool {
		if items[i].Uploading != items[j].Uploading {
			return items[i].Uploading
		}
		return items[i].Expiry < items[j].Expiry
	})

	return items
}

// ErrorIDNotFound is returned from SetExpiry when the item is not found
var ErrorIDNotFound = errors.New("id not found in queue")

// SetExpiry sets the expiry time for an item in the writeback queue.
//
// id should be as returned from the Queue call
//
// The expiry time is set to expiry + relative if expiry is passed in,
// otherwise the expiry of the item is used.
//
// If the item isn't found then it will return ErrorIDNotFound
func (wb *WriteBack) SetExpiry(id Handle, expiry time.Time, relative time.Duration) error {
	wb.mu.Lock()
	defer wb.mu.Unlock()

	wbItem, ok := wb.lookup[id]
	if !ok {
		return ErrorIDNotFound
	}

	// If expiry is not supplied, use expiry of item
	if expiry.IsZero() {
		expiry = wbItem.expiry
	}
	expiry = expiry.Add(relative)

	// Update the expiry with the user requested value
	wb.items._update(wbItem, expiry)
	wb._resetTimer()
	return nil
}
