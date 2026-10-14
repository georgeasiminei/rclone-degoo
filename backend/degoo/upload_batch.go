package degoo

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/rclone/rclone/backend/degoo/api"
)

const (
	uploadAuthorizationBatchMax = 30
	registrationBatchMax        = 100
	uploadBatchWindow           = 150 * time.Millisecond
	uploadBatchRequestTimeout   = 30 * time.Second
)

type uploadAuthResponse struct {
	auth api.AuthData
	err  error
}

type uploadAuthRequest struct {
	ctx    context.Context
	info   api.UploadInfo
	result chan uploadAuthResponse
}

type uploadAuthQueue struct {
	requests []*uploadAuthRequest
	timer    *time.Timer
}

type uploadRegistrationResponse struct {
	ack string
	err error
}

type uploadRegistrationRequest struct {
	ctx    context.Context
	info   api.RegistrationInfo
	result chan uploadRegistrationResponse
}

type uploadRegistrationQueue struct {
	requests []*uploadRegistrationRequest
	timer    *time.Timer
}

type uploadBatchCoordinator struct {
	fs *Fs

	mu             sync.Mutex
	authorizations map[string]*uploadAuthQueue
	registrations  *uploadRegistrationQueue
}

type pathLockManager struct {
	mu    sync.Mutex
	locks map[string]*pathLock
}

type pathLock struct {
	semaphore chan struct{}
	refs      int
}

func newPathLockManager() *pathLockManager {
	return &pathLockManager{locks: make(map[string]*pathLock)}
}

func (m *pathLockManager) acquire(ctx context.Context, remote string) (func(), error) {
	m.mu.Lock()
	lock := m.locks[remote]
	if lock == nil {
		lock = &pathLock{semaphore: make(chan struct{}, 1)}
		m.locks[remote] = lock
	}
	lock.refs++
	m.mu.Unlock()

	select {
	case lock.semaphore <- struct{}{}:
		return func() {
			<-lock.semaphore
			m.releaseRef(remote, lock)
		}, nil
	case <-ctx.Done():
		m.releaseRef(remote, lock)
		return nil, ctx.Err()
	}
}

func (m *pathLockManager) acquireMany(ctx context.Context, remotes ...string) (func(), error) {
	paths := append([]string(nil), remotes...)
	sort.Strings(paths)
	unique := paths[:0]
	for _, remote := range paths {
		if len(unique) == 0 || unique[len(unique)-1] != remote {
			unique = append(unique, remote)
		}
	}
	var releases []func()
	for _, remote := range unique {
		release, err := m.acquire(ctx, remote)
		if err != nil {
			for i := len(releases) - 1; i >= 0; i-- {
				releases[i]()
			}
			return nil, err
		}
		releases = append(releases, release)
	}
	return func() {
		for i := len(releases) - 1; i >= 0; i-- {
			releases[i]()
		}
	}, nil
}

func (m *pathLockManager) releaseRef(remote string, lock *pathLock) {
	m.mu.Lock()
	defer m.mu.Unlock()
	lock.refs--
	if lock.refs == 0 && m.locks[remote] == lock {
		delete(m.locks, remote)
	}
}

func newUploadBatchCoordinator(f *Fs) *uploadBatchCoordinator {
	return &uploadBatchCoordinator{
		fs:             f,
		authorizations: make(map[string]*uploadAuthQueue),
	}
}

func (c *uploadBatchCoordinator) getWriteAuth(ctx context.Context, parentID string, info api.UploadInfo) (api.AuthData, error) {
	if err := ctx.Err(); err != nil {
		return api.AuthData{}, err
	}
	request := &uploadAuthRequest{ctx: ctx, info: info, result: make(chan uploadAuthResponse, 1)}
	c.mu.Lock()
	queue := c.authorizations[parentID]
	if queue == nil {
		queue = &uploadAuthQueue{}
		c.authorizations[parentID] = queue
		queue.timer = time.AfterFunc(uploadBatchWindow, func() { c.flushAuthorizations(parentID, queue) })
	}
	queue.requests = append(queue.requests, request)
	var dispatch []*uploadAuthRequest
	if len(queue.requests) >= uploadAuthorizationBatchMax {
		delete(c.authorizations, parentID)
		queue.timer.Stop()
		dispatch = liveAuthorizationRequests(queue.requests)
	}
	c.mu.Unlock()
	if len(dispatch) != 0 {
		time.AfterFunc(0, func() { c.dispatchAuthorizations(parentID, dispatch) })
	}

	select {
	case response := <-request.result:
		return response.auth, response.err
	case <-ctx.Done():
		return api.AuthData{}, ctx.Err()
	}
}

func (c *uploadBatchCoordinator) register(ctx context.Context, info api.RegistrationInfo) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	request := &uploadRegistrationRequest{ctx: ctx, info: info, result: make(chan uploadRegistrationResponse, 1)}
	c.mu.Lock()
	queue := c.registrations
	if queue == nil {
		queue = &uploadRegistrationQueue{}
		c.registrations = queue
		queue.timer = time.AfterFunc(uploadBatchWindow, func() { c.flushRegistrations(queue) })
	}
	queue.requests = append(queue.requests, request)
	var dispatch []*uploadRegistrationRequest
	if len(queue.requests) >= registrationBatchMax {
		c.registrations = nil
		queue.timer.Stop()
		dispatch = liveRegistrationRequests(queue.requests)
	}
	c.mu.Unlock()
	if len(dispatch) != 0 {
		time.AfterFunc(0, func() { c.dispatchRegistrations(dispatch) })
	}

	select {
	case response := <-request.result:
		return response.ack, response.err
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func liveAuthorizationRequests(requests []*uploadAuthRequest) []*uploadAuthRequest {
	live := make([]*uploadAuthRequest, 0, len(requests))
	for _, request := range requests {
		if request.ctx.Err() == nil {
			live = append(live, request)
		}
	}
	return live
}

func liveRegistrationRequests(requests []*uploadRegistrationRequest) []*uploadRegistrationRequest {
	live := make([]*uploadRegistrationRequest, 0, len(requests))
	for _, request := range requests {
		if request.ctx.Err() == nil {
			live = append(live, request)
		}
	}
	return live
}

func (c *uploadBatchCoordinator) flushAuthorizations(parentID string, queue *uploadAuthQueue) {
	c.mu.Lock()
	if c.authorizations[parentID] != queue {
		c.mu.Unlock()
		return
	}
	delete(c.authorizations, parentID)
	requests := liveAuthorizationRequests(queue.requests)
	c.mu.Unlock()
	if len(requests) != 0 {
		c.dispatchAuthorizations(parentID, requests)
	}
}

func (c *uploadBatchCoordinator) flushRegistrations(queue *uploadRegistrationQueue) {
	c.mu.Lock()
	if c.registrations != queue {
		c.mu.Unlock()
		return
	}
	c.registrations = nil
	requests := liveRegistrationRequests(queue.requests)
	c.mu.Unlock()
	if len(requests) != 0 {
		c.dispatchRegistrations(requests)
	}
}

func batchRequestContext(contexts []context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(contexts[0]), uploadBatchRequestTimeout)
	var mu sync.Mutex
	active := len(contexts)
	stops := make([]func() bool, 0, len(contexts))
	for _, callerCtx := range contexts {
		stops = append(stops, context.AfterFunc(callerCtx, func() {
			mu.Lock()
			active--
			if active == 0 {
				cancel()
			}
			mu.Unlock()
		}))
	}
	return ctx, func() {
		cancel()
		for _, stop := range stops {
			stop()
		}
	}
}

func (c *uploadBatchCoordinator) dispatchAuthorizations(parentID string, requests []*uploadAuthRequest) {
	if len(requests) == 0 {
		return
	}
	infos := make([]api.UploadInfo, len(requests))
	for i, request := range requests {
		infos[i] = request.info
	}
	callerContexts := make([]context.Context, len(requests))
	for i, request := range requests {
		callerContexts[i] = request.ctx
	}
	ctx, cancel := batchRequestContext(callerContexts)
	results, err := c.fs.api.GetWriteAuthBatch(ctx, parentID, infos)
	cancel()
	if err == nil && len(results) != len(requests) {
		err = errors.New("degoo: upload authorization batch returned an unexpected result count")
	}
	for i, request := range requests {
		response := uploadAuthResponse{err: err}
		if err == nil {
			response.auth = results[i].AuthData
			response.err = results[i].Err
		}
		request.result <- response
	}
}

func (c *uploadBatchCoordinator) dispatchRegistrations(requests []*uploadRegistrationRequest) {
	if len(requests) == 0 {
		return
	}
	infos := make([]api.RegistrationInfo, len(requests))
	for i, request := range requests {
		infos[i] = request.info
	}
	callerContexts := make([]context.Context, len(requests))
	for i, request := range requests {
		callerContexts[i] = request.ctx
	}
	ctx, cancel := batchRequestContext(callerContexts)
	ack, err := c.fs.api.RegisterBatch(ctx, infos)
	cancel()
	for _, request := range requests {
		request.result <- uploadRegistrationResponse{ack: ack, err: err}
	}
}
