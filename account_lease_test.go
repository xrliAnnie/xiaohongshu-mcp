package main

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type leaseSessionFixture struct {
	mu       sync.Mutex
	id       string
	closes   atomic.Int32
	closeErr error
}

func (s *leaseSessionFixture) selfAccount(context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.id, nil
}
func (s *leaseSessionFixture) Close() error { s.closes.Add(1); return s.closeErr }
func leaseManagerFixture(t *testing.T) *accountLeaseManager {
	t.Helper()
	m, err := newAccountLeaseManager("account-a", 1, "generation-a")
	if err != nil {
		t.Fatal(err)
	}
	return m
}
func TestAccountLeaseExclusiveSameSessionAndIdentityRecheck(t *testing.T) {
	m := leaseManagerFixture(t)
	s := &leaseSessionFixture{id: "account-a"}
	var opens atomic.Int32
	open := func(context.Context) (accountLeaseSession, error) { opens.Add(1); return s, nil }
	lease, err := m.prepare(context.Background(), strings.Repeat("a", 64), time.Minute, open)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	if _, err = m.prepare(context.Background(), strings.Repeat("b", 64), time.Minute, open); !errors.Is(err, errAccountBusy) {
		t.Fatal("second prepare was not busy", err)
	}
	if opens.Load() != 1 {
		t.Fatal("opened second browser")
	}
	if err = lease.recheck(context.Background()); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	s.id = "account-b"
	s.mu.Unlock()
	if err = lease.recheck(context.Background()); !errors.Is(err, errAccountMismatch) {
		t.Fatal("accepted changed account", err)
	}
	if err = lease.Close(); err != nil {
		t.Fatal(err)
	}
	if err = lease.Close(); err != nil || s.closes.Load() != 1 {
		t.Fatal("non-idempotent cleanup")
	}
	if err = lease.recheck(context.Background()); err == nil {
		t.Fatal("accepted closed lease")
	}
}
func TestAccountLeaseDeadlineAndFailedCleanupKeepExclusion(t *testing.T) {
	m := leaseManagerFixture(t)
	s := &leaseSessionFixture{id: "account-a", closeErr: errors.New("synthetic failure")}
	lease, err := m.prepare(context.Background(), strings.Repeat("a", 64), 20*time.Millisecond, func(context.Context) (accountLeaseSession, error) { return s, nil })
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-lease.done:
	case <-time.After(2 * time.Second):
		t.Fatal("deadline did not clean up")
	}
	if s.closes.Load() != 1 {
		t.Fatal("missing close")
	}
	if _, err = m.prepare(context.Background(), strings.Repeat("a", 64), time.Second, func(context.Context) (accountLeaseSession, error) {
		t.Fatal("opened after failed cleanup")
		return nil, nil
	}); !errors.Is(err, errAccountBusy) {
		t.Fatal("released uncertain session", err)
	}
}
func TestAccountLeaseReservesBeforeOpenAndCancelsInFlight(t *testing.T) {
	m := leaseManagerFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	release := make(chan struct{})
	returned := make(chan error, 1)
	s := &leaseSessionFixture{id: "account-a"}
	go func() {
		_, err := m.prepare(ctx, strings.Repeat("a", 64), time.Minute, func(context.Context) (accountLeaseSession, error) { close(started); <-release; return s, nil })
		returned <- err
	}()
	<-started
	if _, err := m.prepare(context.Background(), strings.Repeat("b", 64), time.Minute, func(context.Context) (accountLeaseSession, error) { t.Error("second opener ran"); return nil, nil }); !errors.Is(err, errAccountBusy) {
		t.Fatal(err)
	}
	cancel()
	close(release)
	if err := <-returned; err == nil {
		t.Fatal("returned expired lease")
	}
	if s.closes.Load() != 1 {
		t.Fatal("late session not closed")
	}
}
func TestAccountLeaseRejectsMissingSelfAndBoundsBeforeOpen(t *testing.T) {
	for _, duration := range []time.Duration{0, -time.Second, 121 * time.Second} {
		m := leaseManagerFixture(t)
		if _, err := m.prepare(context.Background(), strings.Repeat("a", 64), duration, func(context.Context) (accountLeaseSession, error) {
			t.Error("invalid duration opened browser")
			return nil, nil
		}); err == nil {
			t.Fatal("accepted duration")
		}
	}
	m := leaseManagerFixture(t)
	s := &leaseSessionFixture{}
	if _, err := m.prepare(context.Background(), strings.Repeat("a", 64), time.Second, func(context.Context) (accountLeaseSession, error) { return s, nil }); !errors.Is(err, errAccountMismatch) {
		t.Fatal("accepted no stable self", err)
	}
	if s.closes.Load() != 1 {
		t.Fatal("identity failure not cleaned")
	}
}

func TestAccountLeaseSessionReplacementInvalidatesAndSerializes(t *testing.T) {
	m := leaseManagerFixture(t)
	s := &leaseSessionFixture{id: "account-a"}
	lease, err := m.prepare(context.Background(), strings.Repeat("a", 64), time.Minute, func(context.Context) (accountLeaseSession, error) { return s, nil })
	if err != nil {
		t.Fatal(err)
	}
	err = m.replaceSession(context.Background(), func(ctx context.Context, epoch int64) error {
		if epoch != 2 || s.closes.Load() != 1 {
			t.Fatal("replacement before invalidation")
		}
		if lease.recheck(ctx) == nil {
			t.Fatal("old lease valid during replacement")
		}
		if _, err := m.prepare(ctx, strings.Repeat("a", 64), time.Second, func(context.Context) (accountLeaseSession, error) {
			t.Fatal("opened while cookies changing")
			return nil, nil
		}); !errors.Is(err, errAccountBusy) {
			t.Fatal(err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	next, err := m.prepare(context.Background(), strings.Repeat("a", 64), time.Minute, func(context.Context) (accountLeaseSession, error) { return &leaseSessionFixture{id: "account-a"}, nil })
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close()
	if next.epoch != 2 || next.id == lease.id {
		t.Fatal("reused old identity")
	}
}
func TestAccountLeaseUncertainReplacementPoisonsAccount(t *testing.T) {
	m := leaseManagerFixture(t)
	if m.replaceSession(context.Background(), func(context.Context, int64) error { return errors.New("persist/cookie failure") }) == nil {
		t.Fatal("accepted failed replacement")
	}
	if _, err := m.prepare(context.Background(), strings.Repeat("a", 64), time.Second, func(context.Context) (accountLeaseSession, error) {
		t.Fatal("opened uncertain account")
		return nil, nil
	}); !errors.Is(err, errAccountBusy) {
		t.Fatal(err)
	}
}
