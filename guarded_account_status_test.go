package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strings"
	"testing"
)

func TestGuardedAccountStatusUsesExclusiveSelfSessionAndNoMutation(t *testing.T) {
	s, _, _, calls := guardedServiceFixture(t)
	current, _, _ := s.config.Epochs.current()
	session := &leaseSessionFixture{id: current.AccountUserID}
	s.open = func(context.Context, string, frozenAccount) (accountLeaseSession, error) { return session, nil }
	status, err := s.accountStatus(context.Background())
	if err != nil || !status.LoggedIn || status.Account != current || status.Upstream != s.config.Upstream {
		t.Fatal("account proof", err)
	}
	if session.closes.Load() != 1 || calls.Load() != 0 {
		t.Fatal("owner not closed or read mutated")
	}
	s.manager.mu.Lock()
	active := s.manager.active
	s.manager.mu.Unlock()
	if active != nil {
		t.Fatal("read lease remained active")
	}
}
func TestGuardedAccountStatusRejectsChangedIdentityAndFailedCleanup(t *testing.T) {
	for _, closeFailure := range []bool{false, true} {
		t.Run(map[bool]string{false: "wrong-self", true: "cleanup"}[closeFailure], func(t *testing.T) {
			s, _, _, calls := guardedServiceFixture(t)
			current, _, _ := s.config.Epochs.current()
			session := &leaseSessionFixture{id: "different-account"}
			if closeFailure {
				session.id = current.AccountUserID
				session.closeErr = errors.New("cleanup")
			}
			s.open = func(context.Context, string, frozenAccount) (accountLeaseSession, error) { return session, nil }
			if _, err := s.accountStatus(context.Background()); err == nil {
				t.Fatal("accepted unproven account")
			}
			if calls.Load() != 0 {
				t.Fatal("read mutated")
			}
		})
	}
}
func TestGuardedAccountStatusDoesNotStealPreparedWriteLease(t *testing.T) {
	s, raw, digest, calls := guardedServiceFixture(t)
	if _, err := s.prepare(context.Background(), raw, digest, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.accountStatus(context.Background()); err != errAccountBusy {
		t.Fatal("did not preserve account exclusion", err)
	}
	if calls.Load() != 0 {
		t.Fatal("read dispatched")
	}
}
func TestGuardedAccountStatusPrivateHTTPProjection(t *testing.T) {
	s, _, _, calls := guardedServiceFixture(t)
	_, client := privateProviderFixture(t, uint32(os.Geteuid()), guardedRoutes(s, uint32(os.Geteuid())))
	response, err := client.Post("http://private/v1/account", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var value guardedAccountStatus
	if response.StatusCode != http.StatusOK || json.NewDecoder(response.Body).Decode(&value) != nil || !value.LoggedIn || calls.Load() != 0 {
		t.Fatal("bad private account status", response.StatusCode)
	}
}

func TestGuardedAccountStatusWithoutCookiesIsNotLoginProof(t *testing.T) {
	s, _, _, calls := guardedServiceFixture(t)
	epochs, _ := epochFixture(t)
	s.config.Epochs = epochs
	s.open = func(context.Context, string, frozenAccount) (accountLeaseSession, error) {
		t.Fatal("opened browser without controlled cookies")
		return nil, errAccountMismatch
	}
	status, err := s.accountStatus(context.Background())
	if err != nil || status.LoggedIn || calls.Load() != 0 {
		t.Fatal("missing cookies became login proof", err)
	}
}
