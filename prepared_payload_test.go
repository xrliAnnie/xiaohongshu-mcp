package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"testing"
)

func preparedFixture(t *testing.T) (*preparedPayload, *providerArtifact, []byte, providerPayloadPolicy) {
	t.Helper()
	s, d, data := artifactFixture(t)
	a, err := s.importArtifact(context.Background(), d, bytes.NewReader(data), func(context.Context, string, string) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	v := frozenVectors(t)[0]
	var w frozenWrite
	if json.Unmarshal([]byte(v.Canonical), &w) != nil {
		t.Fatal("fixture")
	}
	w.Media = []frozenArtifact{d}
	raw, err := canonicalFrozenWrite(w)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(append([]byte("flywheel:xhs-write:v1\n"), raw...))
	policy := providerPayloadPolicy{Account: w.Account, Upstream: w.Upstream}
	p, err := newPreparedPayload(raw, hex.EncodeToString(sum[:]), policy, []*providerArtifact{a}, v.Now)
	if err != nil {
		t.Fatal(err)
	}
	return p, a, raw, policy
}
func TestPreparedPayloadCopiesWireAndPinsPolicyAndMedia(t *testing.T) {
	p, a, raw, policy := preparedFixture(t)
	raw[0] = 'x'
	if _, _, err := p.snapshot(1789416000000); err != nil {
		t.Fatal("caller mutated private copy", err)
	}
	bad := policy
	bad.Upstream.BinarySHA256 = string(bytes.Repeat([]byte{'f'}, 64))
	if _, err := newPreparedPayload(p.raw, p.digest, bad, []*providerArtifact{a}, 1789416000000); err == nil {
		t.Fatal("accepted wrong deployed policy")
	}
	if _, err := newPreparedPayload(p.raw, p.digest, policy, nil, 1789416000000); err == nil {
		t.Fatal("accepted missing media")
	}
	if err := os.WriteFile(a.path(), []byte("changed"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := p.snapshot(1789416000000); err == nil {
		t.Fatal("accepted changed file")
	}
}
func TestPreparedPayloadWiresActualVerifierIntoCommit(t *testing.T) {
	for _, change := range []bool{false, true} {
		p, a, _, _ := preparedFixture(t)
		e, raw, _, calls := executionFixture(t)
		e.lease.digest = p.digest
		var permit internalPermit
		if json.Unmarshal(raw, &permit) != nil {
			t.Fatal("permit")
		}
		permit.ContentDigest = p.digest
		permit.ProposalID = p.proposalID
		raw, err := canonicalInternalPermit(permit)
		if err != nil {
			t.Fatal(err)
		}
		mac := hmac.New(sha256.New, e.key)
		mac.Write([]byte(permitDomain))
		mac.Write(raw)
		assembled, err := newPayloadExecution(p, e.lease, e.journal, providerExecutionPolicy{Audience: e.audience, KeyID: e.keyID, Key: e.key}, e.admit, func(ctx context.Context, v *verifiedWrite, w frozenWrite, paths []string) error {
			if len(paths) != 1 || paths[0] != a.path() || w.ProposalID != p.proposalID {
				t.Fatal("dispatch did not use stored payload")
			}
			return e.dispatch(ctx, v)
		})
		if err != nil {
			t.Fatal(err)
		}
		if change {
			if os.WriteFile(a.path(), []byte("changed"), 0600) != nil {
				t.Fatal("mutate")
			}
		}
		state := assembled.commit(context.Background(), raw, hex.EncodeToString(mac.Sum(nil)))
		if change {
			if state != "denied" || calls.Load() != 0 {
				t.Fatal("changed bytes reached dispatch", state)
			}
		} else if state != "succeeded" || calls.Load() != 1 {
			t.Fatal("valid stored payload rejected", state)
		}
	}
}
