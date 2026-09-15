package main

import (
	"bytes"
	"context"
	"sync"
	"sync/atomic"
)

type verifiedWrite struct {
	lease  *accountLease
	permit internalPermit
	used   atomic.Bool
}

// Business writers must take this capability immediately before their single
// external action. It is minted only after durable provider consumption.
func (v *verifiedWrite) take() bool {
	return v != nil && v.lease != nil && v.lease.ctx.Err() == nil && v.used.CompareAndSwap(false, true)
}

type privateExecution struct {
	mu                          sync.Mutex
	lease                       *accountLease
	journal                     *writeJournal
	key                         []byte
	audience, keyID, proposalID string
	now                         func() int64
	// Trusted adapters over the stored frozen payload and private authority socket.
	// They are never provided through request JSON or a model callback.
	verifyPayload    func(context.Context) error
	admit            func(context.Context, internalPermit) error
	dispatch         func(context.Context, *verifiedWrite) error
	raw              []byte
	signature, state string
}

func (e *privateExecution) commit(ctx context.Context, raw []byte, signature string) (state string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.state != "" {
		if bytes.Equal(raw, e.raw) && signature == e.signature {
			return e.state
		}
		return "denied"
	}
	state = "denied"
	consumed := false
	defer func() {
		if recover() != nil {
			if consumed {
				state = "unknown"
			} else {
				state = "denied"
			}
		}
		e.raw = append([]byte(nil), raw...)
		e.signature = signature
		e.state = state
	}()
	if e.lease == nil || e.journal == nil || e.now == nil || e.verifyPayload == nil || e.admit == nil || e.dispatch == nil {
		return state
	}
	lease := e.lease
	defer lease.Close()
	lease.operation.Lock()
	defer lease.operation.Unlock()
	expected := permitExpectation{Audience: e.audience, KeyID: e.keyID, ProposalID: e.proposalID, ContentDigest: lease.digest, AccountUserID: lease.accountID, AccountEpoch: lease.epoch, Generation: lease.generation, LeaseID: lease.id, LeaseExpiresAt: lease.expiresAt.UnixMilli()}
	permit, err := verifyInternalPermit(raw, signature, e.key, expected, e.now())
	if err != nil || ctx.Err() != nil || lease.ctx.Err() != nil {
		return state
	}
	callCtx, cancel := context.WithCancel(lease.ctx)
	stop := context.AfterFunc(ctx, cancel)
	defer stop()
	defer cancel()
	if err = e.verifyPayload(callCtx); err != nil {
		return state
	}
	// The authority admission transaction is the switch/revocation ordering point.
	if err = e.admit(callCtx, permit); err != nil {
		return state
	}
	if err = lease.recheckLocked(callCtx); err != nil {
		return state
	}
	// Rehash the frozen bytes after admission and account reads as well.
	if err = e.verifyPayload(callCtx); err != nil {
		return state
	}
	// Revalidate time after every asynchronous preflight has finished.
	if _, err = verifyInternalPermit(raw, signature, e.key, expected, e.now()); err != nil || callCtx.Err() != nil {
		return state
	}
	winner, err := e.journal.consume(writeTombstone{ReceiptID: permit.ReceiptID, AttemptID: permit.AttemptID, Digest: permit.ContentDigest, Generation: permit.ProviderGeneration})
	if err != nil {
		return state
	}
	if !winner {
		return "unknown"
	}
	consumed = true
	state = "unknown"
	if callCtx.Err() != nil {
		return state
	}
	capability := &verifiedWrite{lease: lease, permit: permit}
	if err = e.dispatch(callCtx, capability); err == nil && capability.used.Load() {
		state = "succeeded"
	}
	return state
}
