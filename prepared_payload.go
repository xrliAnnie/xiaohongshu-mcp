package main

import (
	"context"
	"time"
)

type providerPayloadPolicy struct {
	Account  frozenAccount
	Upstream frozenUpstream
}
type preparedPayload struct {
	raw                []byte
	digest, proposalID string
	policy             providerPayloadPolicy
	media              []providerArtifact
}

// Policy comes from the trusted startup registry. Only providerArtifact handles
// created by the private byte importer may supply media; RPC paths are not accepted.
func newPreparedPayload(raw []byte, digest string, policy providerPayloadPolicy, media []*providerArtifact, now int64) (*preparedPayload, error) {
	w, err := verifyFrozenWrite(raw, digest, now)
	if err != nil {
		return nil, errFrozenWrite
	}
	p := &preparedPayload{raw: append([]byte(nil), raw...), digest: digest, proposalID: w.ProposalID, policy: policy}
	for _, a := range media {
		if a == nil {
			return nil, errProviderArtifact
		}
		p.media = append(p.media, *a)
	}
	if _, _, err = p.snapshot(now); err != nil {
		return nil, err
	}
	return p, nil
}
func (p *preparedPayload) snapshot(now int64) (frozenWrite, []string, error) {
	w, err := verifyFrozenWrite(p.raw, p.digest, now)
	if err != nil || w.ProposalID != p.proposalID || w.Account != p.policy.Account || w.Upstream != p.policy.Upstream || len(w.Media) != len(p.media) {
		return frozenWrite{}, nil, errFrozenWrite
	}
	paths := make([]string, 0, len(p.media))
	for i := range p.media {
		a := &p.media[i]
		if a.descriptor != w.Media[i] || a.recheck() != nil {
			return frozenWrite{}, nil, errProviderArtifact
		}
		paths = append(paths, a.path())
	}
	return w, paths, nil
}

type providerExecutionPolicy struct {
	Audience, KeyID string
	Key             []byte
}

// The only dispatcher inputs are freshly decoded copies of stored canonical
// payload and importer-owned paths. Commit accepts no replacement content/media.
func newPayloadExecution(p *preparedPayload, lease *accountLease, journal *writeJournal, policy providerExecutionPolicy, admit func(context.Context, internalPermit) error, dispatch func(context.Context, *verifiedWrite, frozenWrite, []string) error) (*privateExecution, error) {
	if p == nil || lease == nil || journal == nil || len(policy.Key) != 32 || !frozenID(policy.KeyID) || admit == nil || dispatch == nil {
		return nil, errWritePermit
	}
	w, _, err := p.snapshot(time.Now().UnixMilli())
	if err != nil {
		return nil, err
	}
	if policy.Audience != w.Account.ProviderInstanceID || lease.accountID != w.Account.AccountUserID || lease.epoch != w.Account.AccountEpoch || lease.generation != w.Account.ProviderGeneration || lease.digest != p.digest || journal.generation != lease.generation {
		return nil, errAccountMismatch
	}
	e := &privateExecution{lease: lease, journal: journal, key: append([]byte(nil), policy.Key...), audience: policy.Audience, keyID: policy.KeyID, proposalID: p.proposalID, now: func() int64 { return time.Now().UnixMilli() }, admit: admit}
	e.verifyPayload = func(ctx context.Context) error {
		if ctx.Err() != nil {
			return errFrozenWrite
		}
		_, _, err := p.snapshot(e.now())
		if ctx.Err() != nil {
			return errFrozenWrite
		}
		return err
	}
	e.dispatch = func(ctx context.Context, v *verifiedWrite) error {
		w, paths, err := p.snapshot(e.now())
		if err != nil {
			return err
		}
		if ctx.Err() != nil {
			return errFrozenWrite
		}
		return dispatch(ctx, v, w, paths)
	}
	return e, nil
}
