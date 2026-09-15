package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestLoginRetryAuditPersistsBeforeNextReservation(t *testing.T) {
	store, base := epochFixture(t)
	current, err := store.reserve(base.AccountEpoch + 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.recordExplicitLoginRetry(current); err != nil {
		t.Fatal(err)
	}
	loaded, err := openAccountEpochStore(store.path, base, false)
	if err != nil {
		t.Fatal("audit prevented durable reopen", err)
	}
	observed, cookie, err := loaded.current()
	if err != nil || observed != current || cookie != "" {
		t.Fatal("audit changed account epoch or cookies")
	}
	next, err := loaded.reserve(current.AccountEpoch + 1)
	if err != nil || next.AccountEpoch != current.AccountEpoch+1 {
		t.Fatal("audit prevented next reservation", err)
	}
	raw, err := os.ReadFile(filepath.Join(store.path, loginRetryName(next.AccountEpoch)))
	if err != nil || string(raw) != `{"oldEpoch":2,"newEpoch":3,"cause":"explicit_login_retry"}` {
		t.Fatal("wrong retry receipt", string(raw), err)
	}
}
func TestLoginRetryAuditRejectsWrongScopeCorruptionAndSyncFailure(t *testing.T) {
	store, base := epochFixture(t)
	current, err := store.reserve(base.AccountEpoch + 1)
	if err != nil {
		t.Fatal(err)
	}
	wrong := current
	wrong.ProviderGeneration = "other-generation"
	if store.recordExplicitLoginRetry(wrong) == nil {
		t.Fatal("wrong generation admitted")
	}
	store.syncFile = func(*os.File) error { return errors.New("fixture-sync-failure") }
	if store.recordExplicitLoginRetry(current) == nil {
		t.Fatal("unconfirmed audit durability accepted")
	}
	path := filepath.Join(store.path, loginRetryName(current.AccountEpoch+1))
	if err := os.WriteFile(path, []byte(`{"oldEpoch":2,"newEpoch":99,"cause":"explicit_login_retry"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.current(); err == nil {
		t.Fatal("corrupt retry audit accepted")
	}
}
