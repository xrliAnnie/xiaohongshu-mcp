package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func executionFixture(t *testing.T) (*privateExecution, []byte, string, *atomic.Int32) {
	t.Helper()
	m := leaseManagerFixture(t)
	lease, err := m.prepare(context.Background(), strings.Repeat("a", 64), time.Minute, func(context.Context) (accountLeaseSession, error) { return &leaseSessionFixture{id: "account-a"}, nil })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { lease.Close() })
	journal, err := openWriteJournal(filepath.Join(t.TempDir(), "journal"), "generation-a", true)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UnixMilli()
	p := internalPermit{Purpose: permitPurpose, Issuer: "xhs-authority", Audience: "provider-a", KeyID: "key-a", ProposalID: "proposal-a", ReceiptID: "receipt-a", AttemptID: "attempt-a", ContentDigest: lease.digest, AccountUserID: lease.accountID, AccountEpoch: lease.epoch, ProviderGeneration: lease.generation, LeaseID: lease.id, IssuedAt: now, ExpiresAt: now + 30000}
	key := make([]byte, 32)
	raw, err := canonicalInternalPermit(p)
	if err != nil {
		t.Fatal(err)
	}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(permitDomain))
	mac.Write(raw)
	calls := &atomic.Int32{}
	e := &privateExecution{lease: lease, journal: journal, key: key, audience: p.Audience, keyID: p.KeyID, proposalID: p.ProposalID, now: func() int64 { return time.Now().UnixMilli() }, verifyPayload: func(context.Context) error { return nil }, admit: func(context.Context, internalPermit) error { return nil }}
	e.dispatch = func(ctx context.Context, v *verifiedWrite) error {
		if !v.take() {
			t.Error("missing verified context")
			return errors.New("invalid")
		}
		if v.take() {
			t.Error("context reused")
		}
		data, err := os.ReadFile(filepath.Join(journal.path, "receipt-a.json"))
		if err != nil || len(data) == 0 {
			t.Error("dispatch before tombstone")
		}
		calls.Add(1)
		return nil
	}
	return e, raw, hex.EncodeToString(mac.Sum(nil)), calls
}
func TestPrivateExecutionConcurrentReplayDispatchesOnce(t *testing.T) {
	e, raw, sig, calls := executionFixture(t)
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if s := e.commit(context.Background(), raw, sig); s != "succeeded" {
				t.Errorf("state %s", s)
			}
		}()
	}
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatal("not exactly one dispatch", calls.Load())
	}
}
func TestPrivateExecutionRejectsGuardsBeforeMutation(t *testing.T) {
	for _, kind := range []string{"signature", "payload", "admission", "identity", "journal"} {
		t.Run(kind, func(t *testing.T) {
			e, raw, sig, calls := executionFixture(t)
			switch kind {
			case "signature":
				sig = strings.Repeat("f", 64)
			case "payload":
				e.verifyPayload = func(context.Context) error { return errors.New("synthetic payload failure") }
			case "admission":
				e.admit = func(context.Context, internalPermit) error { return errors.New("off") }
			case "identity":
				s := e.lease.session.(*leaseSessionFixture)
				s.mu.Lock()
				s.id = "account-b"
				s.mu.Unlock()
			case "journal":
				e.journal.syncFile = func(*os.File) error { return errors.New("fsync") }
			}
			if state := e.commit(context.Background(), raw, sig); state != "denied" {
				t.Fatal(state)
			}
			if calls.Load() != 0 {
				t.Fatal("mutated despite failed guard")
			}
		})
	}
}
func TestPrivateExecutionResponseLossAndPanicNeverRetry(t *testing.T) {
	for _, panicNow := range []bool{false, true} {
		e, raw, sig, calls := executionFixture(t)
		e.dispatch = func(ctx context.Context, v *verifiedWrite) error {
			if !v.take() {
				t.Fatal("no verified context")
			}
			calls.Add(1)
			if panicNow {
				panic("synthetic")
			}
			return errors.New("synthetic response lost")
		}
		if e.commit(context.Background(), raw, sig) != "unknown" || e.commit(context.Background(), raw, sig) != "unknown" || calls.Load() != 1 {
			t.Fatal("unknown retried")
		}
	}
}
func TestPrivateExecutionExistingTombstoneNeverDispatches(t *testing.T) {
	e, raw, sig, calls := executionFixture(t)
	if ok, err := e.journal.consume(writeTombstone{ReceiptID: "receipt-a", AttemptID: "attempt-a", Digest: e.lease.digest, Generation: e.lease.generation}); !ok || err != nil {
		t.Fatal(err)
	}
	if e.commit(context.Background(), raw, sig) != "unknown" || calls.Load() != 0 {
		t.Fatal("replayed persisted consumption")
	}
}

func TestPrivateExecutionRevalidatesAfterAdmissionAwait(t *testing.T) {
	for _, kind := range []string{"account", "expiry", "payload"} {
		t.Run(kind, func(t *testing.T) {
			e, raw, sig, calls := executionFixture(t)
			changed := false
			initialNow := e.now()
			e.verifyPayload = func(context.Context) error {
				if changed && kind == "payload" {
					return errors.New("changed frozen bytes")
				}
				return nil
			}
			e.admit = func(context.Context, internalPermit) error {
				changed = true
				if kind == "account" {
					s := e.lease.session.(*leaseSessionFixture)
					s.mu.Lock()
					s.id = "account-b"
					s.mu.Unlock()
				}
				if kind == "expiry" {
					e.now = func() int64 { return initialNow + 60000 }
				}
				return nil
			}
			if e.commit(context.Background(), raw, sig) != "denied" || calls.Load() != 0 {
				t.Fatal("accepted changed state after await")
			}
		})
	}
}
