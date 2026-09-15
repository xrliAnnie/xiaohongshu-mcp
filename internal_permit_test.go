package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

type permitVector struct {
	KeyHex         string         `json:"keyHex"`
	Now            int64          `json:"now"`
	LeaseExpiresAt int64          `json:"leaseExpiresAt"`
	Canonical      string         `json:"canonical"`
	Signature      string         `json:"signature"`
	Permit         internalPermit `json:"permit"`
}

func loadPermitVector(t *testing.T) (permitVector, []byte, permitExpectation) {
	t.Helper()
	data, err := os.ReadFile("testdata/xhs-permit-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	var v permitVector
	if err = json.Unmarshal(data, &v); err != nil {
		t.Fatal(err)
	}
	key, err := hex.DecodeString(v.KeyHex)
	if err != nil {
		t.Fatal(err)
	}
	p := v.Permit
	return v, key, permitExpectation{Audience: p.Audience, KeyID: p.KeyID, ProposalID: p.ProposalID, ContentDigest: p.ContentDigest, AccountUserID: p.AccountUserID, AccountEpoch: p.AccountEpoch, Generation: p.ProviderGeneration, LeaseID: p.LeaseID, LeaseExpiresAt: v.LeaseExpiresAt}
}
func TestGuardedWritePermitGoldenVector(t *testing.T) {
	v, key, expected := loadPermitVector(t)
	p, err := verifyInternalPermit([]byte(v.Canonical), v.Signature, key, expected, v.Now)
	if err != nil || p != v.Permit {
		t.Fatalf("golden verification failed: %v", err)
	}
	encoded, err := canonicalInternalPermit(p)
	if err != nil || string(encoded) != v.Canonical {
		t.Fatal("canonical differs from TS/Python fixture")
	}
}
func TestGuardedWritePermitBindsProviderLeaseAndAccount(t *testing.T) {
	v, key, expected := loadPermitVector(t)
	changes := []func(*permitExpectation){func(e *permitExpectation) { e.Audience = "other" }, func(e *permitExpectation) { e.KeyID = "other" }, func(e *permitExpectation) { e.ProposalID = "other" }, func(e *permitExpectation) { e.ContentDigest = strings.Repeat("b", 64) }, func(e *permitExpectation) { e.AccountUserID = "other" }, func(e *permitExpectation) { e.AccountEpoch++ }, func(e *permitExpectation) { e.Generation = "other" }, func(e *permitExpectation) { e.LeaseID = "other" }, func(e *permitExpectation) { e.LeaseExpiresAt = v.Now }}
	for i, change := range changes {
		e := expected
		change(&e)
		if _, err := verifyInternalPermit([]byte(v.Canonical), v.Signature, key, e, v.Now); err == nil {
			t.Fatalf("accepted changed binding %d", i)
		}
	}
	for _, now := range []int64{v.Now - 1, v.Permit.ExpiresAt, v.Permit.ExpiresAt + 1} {
		if _, err := verifyInternalPermit([]byte(v.Canonical), v.Signature, key, expected, now); err == nil {
			t.Fatalf("accepted time %d", now)
		}
	}
}
func TestGuardedWritePermitRejectsAmbiguousWireAndBadKeys(t *testing.T) {
	v, key, expected := loadPermitVector(t)
	for _, raw := range []string{" " + v.Canonical, strings.Replace(v.Canonical, `"issuer":"xhs-authority"`, `"issuer":"xhs-authority","issuer":"xhs-authority"`, 1), strings.Replace(v.Canonical, `"accountEpoch":3`, `"accountEpoch":3.0`, 1), strings.Replace(v.Canonical, `"attemptId":"attempt-a"`, `"attemptId":"changed"`, 1)} {
		if _, err := verifyInternalPermit([]byte(raw), v.Signature, key, expected, v.Now); err == nil {
			t.Fatal("accepted ambiguous/tampered wire")
		}
	}
	for _, badKey := range [][]byte{nil, []byte("short"), make([]byte, 32)} {
		if _, err := verifyInternalPermit([]byte(v.Canonical), v.Signature, badKey, expected, v.Now); err == nil {
			t.Fatal("accepted bad key")
		}
	}
	for _, signature := range []string{"", strings.Repeat("x", 64), strings.Repeat("0", 64)} {
		if _, err := verifyInternalPermit([]byte(v.Canonical), signature, key, expected, v.Now); err == nil {
			t.Fatal("accepted bad signature")
		}
	}
}
func TestGuardedWritePermitRejectsSignedWrongPurposeAndLongWindow(t *testing.T) {
	v, key, expected := loadPermitVector(t)
	expected.LeaseExpiresAt = v.Now + 180000
	for _, change := range []func(*internalPermit){func(p *internalPermit) { p.Purpose = "ship" }, func(p *internalPermit) { p.Issuer = "bridge" }, func(p *internalPermit) { p.ExpiresAt = p.IssuedAt + 60001 }, func(p *internalPermit) { p.AccountEpoch = -1 }} {
		p := v.Permit
		change(&p)
		raw, err := canonicalInternalPermit(p)
		if err != nil {
			continue
		}
		mac := hmac.New(sha256.New, key)
		mac.Write([]byte("flywheel:xhs-permit:v1\n"))
		mac.Write(raw)
		if _, err = verifyInternalPermit(raw, hex.EncodeToString(mac.Sum(nil)), key, expected, v.Now); err == nil {
			t.Fatal("accepted signed invalid authority permit")
		}
	}
}
