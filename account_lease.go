package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"sync"
	"time"
)

var errAccountBusy = errors.New("account_busy")
var errAccountMismatch = errors.New("founder_account_mismatch")

// Implemented only by the trusted same-page browser owner, never an RPC caller.
type accountLeaseSession interface {
	selfAccount(context.Context) (string, error)
	Close() error
}
type accountLeaseManager struct {
	mu                    sync.Mutex
	accountID, generation string
	epoch                 int64
	active                *accountLease
	changing              bool
}
type accountLease struct {
	manager                           *accountLeaseManager
	id, digest, accountID, generation string
	epoch                             int64
	expiresAt                         time.Time
	ctx                               context.Context
	cancel                            context.CancelFunc
	session                           accountLeaseSession
	ready, done                       chan struct{}
	once                              sync.Once
	operation                         sync.Mutex
	closeErr                          error
}

func newAccountLeaseManager(account string, epoch int64, generation string) (*accountLeaseManager, error) {
	if account == "" || epoch < 1 || epoch > maxPermitInteger || !journalID.MatchString(generation) {
		return nil, errAccountMismatch
	}
	return &accountLeaseManager{accountID: account, epoch: epoch, generation: generation}, nil
}
func (m *accountLeaseManager) prepare(ctx context.Context, digest string, duration time.Duration, open func(context.Context) (accountLeaseSession, error)) (*accountLease, error) {
	if ctx.Err() != nil || !journalDigest.MatchString(digest) || duration <= 0 || duration > 120*time.Second || open == nil {
		return nil, errAccountMismatch
	}
	var random [32]byte
	if _, err := rand.Read(random[:]); err != nil {
		return nil, errAccountMismatch
	}
	lifetime, cancel := context.WithTimeout(ctx, duration)
	deadline, _ := lifetime.Deadline()
	m.mu.Lock()
	if m.active != nil || m.changing {
		m.mu.Unlock()
		cancel()
		return nil, errAccountBusy
	}
	lease := &accountLease{manager: m, id: hex.EncodeToString(random[:]), digest: digest, accountID: m.accountID, epoch: m.epoch, generation: m.generation, expiresAt: deadline, ctx: lifetime, cancel: cancel, ready: make(chan struct{}), done: make(chan struct{})}
	m.active = lease
	m.mu.Unlock()
	// Reserve before any await/open. Close waits for ready so a late factory result
	// remains owned even if cancellation happens during browser startup.
	session, err := open(lifetime)
	lease.session = session
	close(lease.ready)
	if err != nil || session == nil {
		lease.Close()
		return nil, errAccountMismatch
	}
	if err = lease.recheck(ctx); err != nil {
		lease.Close()
		return nil, errAccountMismatch
	}
	go func() { <-lifetime.Done(); lease.Close() }()
	return lease, nil
}
func (l *accountLease) recheck(ctx context.Context) error {
	l.operation.Lock()
	defer l.operation.Unlock()
	return l.recheckLocked(ctx)
}
func (l *accountLease) recheckLocked(ctx context.Context) error {
	<-l.ready
	if ctx.Err() != nil || l.ctx.Err() != nil || l.session == nil {
		return errAccountMismatch
	}
	l.manager.mu.Lock()
	current := l.manager.active == l && l.manager.epoch == l.epoch && l.manager.accountID == l.accountID && l.manager.generation == l.generation
	l.manager.mu.Unlock()
	if !current {
		return errAccountMismatch
	}
	// Both caller cancellation and the lease deadline bound identity extraction.
	checkCtx, cancel := context.WithCancel(l.ctx)
	stop := context.AfterFunc(ctx, cancel)
	defer stop()
	defer cancel()
	id, err := l.session.selfAccount(checkCtx)
	if err != nil || id == "" || id != l.accountID || checkCtx.Err() != nil {
		return errAccountMismatch
	}
	return nil
}
func (l *accountLease) Close() error {
	l.once.Do(func() {
		l.cancel()
		<-l.ready
		// Close may interrupt a blocked same-page read. The browser owner must provide
		// concurrent-safe teardown; a failed teardown never releases the account slot.
		if l.session != nil {
			l.closeErr = l.session.Close()
		}
		l.operation.Lock()
		defer l.operation.Unlock()
		l.manager.mu.Lock()
		if l.closeErr == nil && l.manager.active == l {
			l.manager.active = nil
		}
		l.manager.mu.Unlock()
		close(l.done)
	})
	return l.closeErr
}

// The trusted callback must durably persist nextEpoch before replacing any cookie
// or login state. Failure leaves the account closed until supervised recovery;
// restarting must load durable epoch/generation, never reset to default values.
func (m *accountLeaseManager) replaceSession(ctx context.Context, replace func(context.Context, int64) error) error {
	if ctx.Err() != nil || replace == nil {
		return errAccountMismatch
	}
	m.mu.Lock()
	if m.changing || m.epoch >= maxPermitInteger {
		m.mu.Unlock()
		return errAccountBusy
	}
	m.changing = true
	active := m.active
	m.mu.Unlock()
	if active != nil {
		if err := active.Close(); err != nil {
			return errAccountMismatch
		}
	}
	if ctx.Err() != nil {
		return errAccountMismatch
	}
	m.mu.Lock()
	m.epoch++
	epoch := m.epoch
	m.mu.Unlock()
	if err := replace(ctx, epoch); err != nil {
		return errAccountMismatch
	}
	if ctx.Err() != nil {
		return errAccountMismatch
	}
	m.mu.Lock()
	m.changing = false
	m.mu.Unlock()
	return nil
}
