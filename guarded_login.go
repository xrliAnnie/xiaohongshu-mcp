package main

import (
	"context"
	"github.com/go-rod/rod/lib/proto"
	"sync"
	"time"
)

type guardedLoginQR struct {
	Account   frozenAccount  `json:"account"`
	Upstream  frozenUpstream `json:"upstream"`
	Image     string         `json:"image"`
	LoggedIn  bool           `json:"loggedIn"`
	ExpiresAt int64          `json:"expiresAt"`
}
type trackedLoginOwner struct {
	privateLoginSession
	once     sync.Once
	closeErr error
}

func (o *trackedLoginOwner) Close() error {
	o.once.Do(func() { o.closeErr = closePrivateLogin(o.privateLoginSession) })
	return o.closeErr
}

type privateLoginJob struct {
	started     chan struct{}
	startedOnce sync.Once
	mu          sync.Mutex
	ready, done chan struct{}
	readyOnce   sync.Once
	cancel      context.CancelFunc
	result      guardedLoginQR
	err         error
	owner       *trackedLoginOwner
}

func (j *privateLoginJob) await(ctx context.Context) (guardedLoginQR, error) {
	select {
	case <-j.ready:
	case <-ctx.Done():
		return guardedLoginQR{}, errPrivateProvider
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.err != nil || ctx.Err() != nil || (!j.result.LoggedIn && j.result.ExpiresAt <= time.Now().UnixMilli()) {
		return guardedLoginQR{}, errPrivateProvider
	}
	return j.result, nil
}
func (j *privateLoginJob) close() error {
	j.cancel()
	select {
	case <-j.done:
	case <-time.After(30 * time.Second):
		return errPrivateProvider
	}
	if j.owner != nil && j.owner.closeErr != nil {
		return errPrivateProvider
	}
	return nil
}
func (s *guardedService) runLogin(ctx context.Context, j *privateLoginJob) {
	var resultErr error
	defer func() {
		if recover() != nil {
			resultErr = errPrivateProvider
			if j.owner != nil {
				j.owner.Close()
			}
		}
		j.startedOnce.Do(func() { close(j.started) })
		j.mu.Lock()
		j.err = resultErr
		if resultErr == nil {
			j.result.Image = ""
			j.result.LoggedIn = true
		}
		j.mu.Unlock()
		j.readyOnce.Do(func() { close(j.ready) })
		close(j.done)
		j.cancel()
	}()
	resultErr = s.config.Epochs.replace(ctx, s.manager, func(ctx context.Context, account frozenAccount) ([]*proto.NetworkCookieParam, error) {
		j.startedOnce.Do(func() { close(j.started) })
		owner, err := s.openLogin(ctx)
		if owner != nil {
			j.owner = &trackedLoginOwner{privateLoginSession: owner}
		}
		if err != nil || owner == nil {
			if j.owner != nil {
				j.owner.Close()
			}
			return nil, errPrivateProvider
		}
		return obtainPrivateLogin(ctx, account, j.owner, func(ctx context.Context, image string) error {
			if ctx.Err() != nil {
				return errPrivateProvider
			}
			deadline, _ := ctx.Deadline()
			j.mu.Lock()
			j.result = guardedLoginQR{Account: account, Upstream: s.config.Upstream, Image: image, ExpiresAt: deadline.UnixMilli()}
			j.mu.Unlock()
			j.readyOnce.Do(func() { close(j.ready) })
			return nil
		})
	})
}
func (s *guardedService) loginQR(ctx context.Context) (guardedLoginQR, error) {
	if !s.mu.TryLock() {
		return guardedLoginQR{}, errAccountBusy
	}
	if ctx.Err() != nil || s.ctx.Err() != nil {
		s.mu.Unlock()
		return guardedLoginQR{}, errPrivateProvider
	}
	j := s.login
	created := false
	if j == nil {
		s.manager.mu.Lock()
		busy := s.manager.active != nil || s.manager.changing
		s.manager.mu.Unlock()
		if busy {
			s.mu.Unlock()
			return guardedLoginQR{}, errAccountBusy
		}
		_, path, err := s.config.Epochs.current()
		if err != nil {
			s.mu.Unlock()
			return guardedLoginQR{}, errPrivateProvider
		}
		if path != "" {
			s.mu.Unlock()
			status, err := s.accountStatus(ctx)
			return guardedLoginQR{Account: status.Account, Upstream: status.Upstream, LoggedIn: status.LoggedIn}, err
		}
		lifetime, cancel := context.WithTimeout(s.ctx, 4*time.Minute)
		j = &privateLoginJob{started: make(chan struct{}), ready: make(chan struct{}), done: make(chan struct{}), cancel: cancel}
		s.login = j
		created = true
		go s.runLogin(lifetime, j)
		// Keep service admission locked until epoch replacement owns exclusion.
		<-j.started
	}
	s.mu.Unlock()
	result, err := j.await(ctx)
	if err != nil {
		if created && ctx.Err() != nil {
			j.cancel()
		}
		return guardedLoginQR{}, err
	}
	if result.LoggedIn {
		status, err := s.accountStatus(ctx)
		return guardedLoginQR{Account: status.Account, Upstream: status.Upstream, LoggedIn: status.LoggedIn}, err
	}
	return result, nil
}
