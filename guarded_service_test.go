package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/proto"
	"io"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func guardedServiceFixture(t *testing.T) (*guardedService, []byte, string, *atomic.Int32) {
	t.Helper()
	epochs, base := epochFixture(t)
	m, _ := newAccountLeaseManager(base.AccountUserID, base.AccountEpoch, base.ProviderGeneration)
	if err := epochs.replace(context.Background(), m, func(context.Context, frozenAccount) ([]*proto.NetworkCookieParam, error) {
		return []*proto.NetworkCookieParam{{Name: "synthetic", Value: "fixture", Domain: ".xiaohongshu.com", Path: "/"}}, nil
	}); err != nil {
		t.Fatal(err)
	}
	current, _, err := epochs.current()
	if err != nil {
		t.Fatal(err)
	}
	v := frozenVectors(t)[2]
	var w frozenWrite
	if json.Unmarshal([]byte(v.Canonical), &w) != nil {
		t.Fatal("wire")
	}
	w.Account = current
	raw, err := canonicalFrozenWrite(w)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(append([]byte("flywheel:xhs-write:v1\n"), raw...))
	digest := hex.EncodeToString(sum[:])
	journal, err := openWriteJournal(filepath.Join(t.TempDir(), "journal"), current.ProviderGeneration, true)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if os.Chmod(root, 0700) != nil {
		t.Fatal("mode")
	}
	s, err := newGuardedService(context.Background(), guardedServiceConfig{Epochs: epochs, Journal: journal, MediaRoot: root, Upstream: w.Upstream, Execution: providerExecutionPolicy{Audience: current.ProviderInstanceID, KeyID: "key-a", Key: make([]byte, 32)}, Decode: func(context.Context, string, string) error { return nil }, Admit: func(context.Context, internalPermit) error { return nil }, Resolve: func(context.Context, frozenAccount, frozenTarget) (string, error) { return "synthetic-token", nil }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	s.open = func(context.Context, string, frozenAccount) (accountLeaseSession, error) {
		return &guardedSessionFixture{leaseSessionFixture: &leaseSessionFixture{id: current.AccountUserID}, page: &rod.Page{}}, nil
	}
	calls := &atomic.Int32{}
	s.dispatcher.run = func(context.Context, *rod.Page, guardedCommand) error { calls.Add(1); return nil }
	return s, raw, digest, calls
}
func servicePermit(t *testing.T, s *guardedService, lease preparedLease, raw []byte) ([]byte, string) {
	t.Helper()
	var w frozenWrite
	if json.Unmarshal(raw, &w) != nil {
		t.Fatal("wire")
	}
	now := time.Now().UnixMilli()
	p := internalPermit{Purpose: permitPurpose, Issuer: "xhs-authority", Audience: w.Account.ProviderInstanceID, KeyID: s.config.Execution.KeyID, ProposalID: w.ProposalID, ReceiptID: "receipt-a", AttemptID: "attempt-a", ContentDigest: lease.ContentDigest, AccountUserID: lease.AccountUserID, AccountEpoch: lease.AccountEpoch, ProviderGeneration: lease.ProviderGeneration, LeaseID: lease.LeaseID, IssuedAt: now, ExpiresAt: now + 30000}
	wire, err := canonicalInternalPermit(p)
	if err != nil {
		t.Fatal(err)
	}
	mac := hmac.New(sha256.New, s.config.Execution.Key)
	mac.Write([]byte(permitDomain))
	mac.Write(wire)
	return wire, hex.EncodeToString(mac.Sum(nil))
}
func TestGuardedServicePrepareCommitAndRestartStatus(t *testing.T) {
	s, raw, digest, calls := guardedServiceFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	lease, err := s.prepare(ctx, raw, digest, nil)
	if err != nil {
		t.Fatal(err)
	}
	cancel() // HTTP request completion must not destroy the prepared lease.
	if calls.Load() != 0 {
		t.Fatal("prepare mutated")
	}
	if _, err = s.prepare(context.Background(), raw, digest, nil); err != errAccountBusy {
		t.Fatal("second lease accepted", err)
	}
	permit, sig := servicePermit(t, s, lease, raw)
	if state := s.commit(context.Background(), lease.LeaseID, permit, sig); state != "succeeded" {
		t.Fatal(state)
	}
	if state := s.commit(context.Background(), lease.LeaseID, permit, sig); state != "succeeded" || calls.Load() != 1 {
		t.Fatal("replayed", state)
	}
	// Drop only the process cache; durable tombstone status cannot become retryable.
	s.mu.Lock()
	s.executions = map[string]*privateExecution{}
	s.mu.Unlock()
	if s.status("receipt-a", "attempt-a", digest) != "unknown" {
		t.Fatal("lost durable attempt")
	}
	if s.status("receipt-a", "other-attempt", digest) != "denied" {
		t.Fatal("unbound status")
	}
	entries, err := os.ReadDir(s.config.MediaRoot)
	if err != nil || len(entries) != 0 {
		t.Fatal("terminal scratch not cleaned", err)
	}
}
func TestGuardedServiceRejectsBadPermitAndMissingLease(t *testing.T) {
	s, raw, digest, calls := guardedServiceFixture(t)
	lease, err := s.prepare(context.Background(), raw, digest, nil)
	if err != nil {
		t.Fatal(err)
	}
	permit, _ := servicePermit(t, s, lease, raw)
	if s.commit(context.Background(), lease.LeaseID, permit, "bad") != "denied" || calls.Load() != 0 {
		t.Fatal("unsigned write")
	}
	if s.commit(context.Background(), "missing", permit, "bad") != "denied" {
		t.Fatal("missing lease")
	}
}

func TestGuardedServiceRechecksImportedMediaAtCommit(t *testing.T) {
	s, _, _, calls := guardedServiceFixture(t)
	p, artifact, _, _ := preparedFixture(t)
	data, err := os.ReadFile(artifact.path())
	if err != nil {
		t.Fatal(err)
	}
	var w frozenWrite
	if json.Unmarshal(p.raw, &w) != nil {
		t.Fatal("wire")
	}
	w.Account, _, err = s.config.Epochs.current()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := canonicalFrozenWrite(w)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(append([]byte("flywheel:xhs-write:v1\n"), raw...))
	digest := hex.EncodeToString(sum[:])
	lease, err := s.prepare(context.Background(), raw, digest, func(index int) (io.Reader, error) {
		if index == 0 {
			return bytes.NewReader(data), nil
		}
		return nil, io.EOF
	})
	if err != nil {
		t.Fatal(err)
	}
	imported := s.executions[lease.LeaseID].payload.media[0].path()
	if err = os.WriteFile(imported, []byte("changed"), 0600); err != nil {
		t.Fatal(err)
	}
	permit, sig := servicePermit(t, s, lease, raw)
	if s.commit(context.Background(), lease.LeaseID, permit, sig) != "denied" || calls.Load() != 0 {
		t.Fatal("changed media dispatched")
	}
}
func TestGuardedServicePreservesScratchOnUncertainBrowserCleanup(t *testing.T) {
	s, raw, digest, _ := guardedServiceFixture(t)
	s.open = func(context.Context, string, frozenAccount) (accountLeaseSession, error) {
		return &guardedSessionFixture{leaseSessionFixture: &leaseSessionFixture{id: "account-a", closeErr: errors.New("synthetic-cleanup-uncertain")}, page: &rod.Page{}}, nil
	}
	lease, err := s.prepare(context.Background(), raw, digest, nil)
	if err != nil {
		t.Fatal(err)
	}
	permit, _ := servicePermit(t, s, lease, raw)
	if s.commit(context.Background(), lease.LeaseID, permit, "bad") != "denied" {
		t.Fatal("bad signature accepted")
	}
	entries, err := os.ReadDir(s.config.MediaRoot)
	if err != nil || len(entries) != 1 {
		t.Fatal("removed scratch while browser ownership uncertain")
	}
	if _, err = s.prepare(context.Background(), raw, digest, nil); err != errAccountBusy {
		t.Fatal("released uncertain account")
	}
	if s.Close() == nil {
		t.Fatal("hid cleanup error")
	}
}
