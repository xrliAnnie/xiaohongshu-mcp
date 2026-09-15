package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"github.com/go-rod/rod"
	"testing"
	"time"
)

type guardedSessionFixture struct {
	*leaseSessionFixture
	page *rod.Page
}

func (s *guardedSessionFixture) writePage() *rod.Page { return s.page }
func TestGuardedDispatcherRejectsMissingCapability(t *testing.T) {
	d := newGuardedDispatcher(nil)
	if d.dispatch(context.Background(), nil) == nil {
		t.Fatal("accepted absent capability")
	}
	if d.dispatch(context.Background(), &verifiedWrite{}) == nil {
		t.Fatal("accepted empty capability")
	}
}
func TestGuardedDispatcherUsesSameSessionAndBoundComment(t *testing.T) {
	v := frozenVectors(t)[2]
	var w frozenWrite
	if json.Unmarshal([]byte(v.Canonical), &w) != nil {
		t.Fatal("fixture")
	}
	p, err := newPreparedPayload([]byte(v.Canonical), v.Digest, providerPayloadPolicy{Account: w.Account, Upstream: w.Upstream}, nil, time.Now().UnixMilli())
	if err != nil {
		t.Fatal(err)
	}
	m := leaseManagerFixture(t)
	page := &rod.Page{}
	s := &guardedSessionFixture{leaseSessionFixture: &leaseSessionFixture{id: "account-a"}, page: page}
	lease, err := m.prepare(context.Background(), v.Digest, time.Minute, func(context.Context) (accountLeaseSession, error) { return s, nil })
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	tokenCalls, calls := 0, 0
	d := newGuardedDispatcher(func(ctx context.Context, a frozenAccount, target frozenTarget) (string, error) {
		tokenCalls++
		if a != w.Account || target.FeedID != w.Target.FeedID {
			t.Fatal("unbound token resolution")
		}
		return "synthetic-token", nil
	})
	d.run = func(ctx context.Context, actual *rod.Page, command guardedCommand) error {
		calls++
		if actual != page || command.operation != w.OperationID || command.content != *w.Payload.Content || command.feedID != w.Target.FeedID || command.token != "synthetic-token" {
			t.Fatal("changed command or session")
		}
		return nil
	}
	cap := &verifiedWrite{lease: lease, payload: p, permit: internalPermit{ContentDigest: p.digest, AccountUserID: w.Account.AccountUserID, AccountEpoch: w.Account.AccountEpoch, ProviderGeneration: w.Account.ProviderGeneration, LeaseID: lease.id, ExpiresAt: time.Now().Add(time.Minute).UnixMilli()}}
	for _, mutate := range []func(*verifiedWrite){
		func(v *verifiedWrite) { v.permit.ContentDigest = "wrong" },
		func(v *verifiedWrite) { v.permit.AccountUserID = "wrong" },
		func(v *verifiedWrite) { v.permit.AccountEpoch++ },
		func(v *verifiedWrite) { v.permit.ProviderGeneration = "wrong" },
		func(v *verifiedWrite) { v.permit.LeaseID = "wrong" },
	} {
		bad := &verifiedWrite{lease: lease, payload: p, permit: cap.permit}
		mutate(bad)
		if d.dispatch(context.Background(), bad) == nil || calls != 0 || tokenCalls != 0 {
			t.Fatal("unbound capability reached resolver or writer")
		}
	}
	if err = d.dispatch(context.Background(), cap); err != nil {
		t.Fatal(err)
	}
	if d.dispatch(context.Background(), cap) == nil || calls != 1 || tokenCalls != 1 {
		t.Fatal("capability replayed")
	}
}
func TestGuardedCommandPreservesSixFrozenOperations(t *testing.T) {
	for _, v := range frozenVectors(t) {
		var w frozenWrite
		if json.Unmarshal([]byte(v.Canonical), &w) != nil {
			t.Fatal("fixture")
		}
		var paths []string
		for range w.Media {
			paths = append(paths, "/controlled/media")
		}
		c, err := commandForFrozen(w, paths, "synthetic-token")
		if err != nil || c.operation != w.OperationID {
			t.Fatal("operation lost", err)
		}
		if c.image != nil && (c.image.Title != *w.Payload.Title || c.image.Content != *w.Payload.Content || c.image.IsOriginal != *w.Payload.IsOriginal) {
			t.Fatal("image altered")
		}
		if c.video != nil && (c.video.Title != *w.Payload.Title || c.video.Content != *w.Payload.Content || c.video.IsOriginal != *w.Payload.IsOriginal) {
			t.Fatal("video altered")
		}
		if w.Target != nil && (c.feedID != w.Target.FeedID || w.Target.CommentID != nil && c.commentID != *w.Target.CommentID || w.Target.UserID != nil && c.userID != *w.Target.UserID) {
			t.Fatal("target altered")
		}
	}
	// A forged wire digest is never accepted as the stored capability payload.
	v := frozenVectors(t)[2]
	sum := sha256.Sum256([]byte("wrong"))
	if _, err := newPreparedPayload([]byte(v.Canonical), hex.EncodeToString(sum[:]), providerPayloadPolicy{}, nil, v.Now); err == nil {
		t.Fatal("accepted forged content")
	}
}
