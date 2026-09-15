package main

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/go-rod/rod/lib/proto"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
)

func epochFixture(t *testing.T) (*accountEpochStore, frozenAccount) {
	t.Helper()
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	account := frozenAccount{ProviderInstanceID: "provider-a", AccountUserID: "account-a", AccountEpoch: 1, ProviderGeneration: "generation-a"}
	store, err := openAccountEpochStore(filepath.Join(root, "epochs"), account, true)
	if err != nil {
		t.Fatal(err)
	}
	return store, account
}
func TestAccountEpochRestartNeverFallsBackToOldCookies(t *testing.T) {
	store, a := epochFixture(t)
	m, err := newAccountLeaseManager(a.AccountUserID, a.AccountEpoch, a.ProviderGeneration)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	err = store.replace(context.Background(), m, func(ctx context.Context, next frozenAccount) ([]*proto.NetworkCookieParam, error) {
		calls++
		restarted, e := openAccountEpochStore(store.path, a, false)
		if e != nil {
			t.Fatal(e)
		}
		current, cookiePath, e := restarted.current()
		if e != nil || current.AccountEpoch != 2 || cookiePath != "" {
			t.Fatal("epoch not durable before login", current, e)
		}
		return nil, errors.New("synthetic-login-failed")
	})
	if err == nil || calls != 1 {
		t.Fatal("login failure ignored")
	}
	restarted, err := openAccountEpochStore(store.path, a, false)
	if err != nil {
		t.Fatal(err)
	}
	current, path, err := restarted.current()
	if err != nil || current.AccountEpoch != 2 || path != "" {
		t.Fatal("revived prior session")
	}
}
func TestAccountEpochSuccessfulReplacementBindsCookieFile(t *testing.T) {
	store, a := epochFixture(t)
	m, _ := newAccountLeaseManager(a.AccountUserID, a.AccountEpoch, a.ProviderGeneration)
	err := store.replace(context.Background(), m, func(ctx context.Context, next frozenAccount) ([]*proto.NetworkCookieParam, error) {
		return []*proto.NetworkCookieParam{{Name: "synthetic", Value: "test", Domain: ".xiaohongshu.com", Path: "/"}}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	restarted, err := openAccountEpochStore(store.path, a, false)
	if err != nil {
		t.Fatal(err)
	}
	next, path, err := restarted.current()
	if err != nil || next.AccountEpoch != 2 || path == "" {
		t.Fatal("missing durable cookies", err)
	}
	if _, err = loadControlledCookies(path, next); err != nil {
		t.Fatal(err)
	}
	if _, err = loadControlledCookies(path, a); err == nil {
		t.Fatal("old epoch reused")
	}
}
func TestAccountEpochFsyncFailurePreventsLogin(t *testing.T) {
	store, a := epochFixture(t)
	m, _ := newAccountLeaseManager(a.AccountUserID, a.AccountEpoch, a.ProviderGeneration)
	store.syncFile = func(*os.File) error { return errors.New("injected-sync") }
	calls := 0
	if store.replace(context.Background(), m, func(context.Context, frozenAccount) ([]*proto.NetworkCookieParam, error) { calls++; return nil, nil }) == nil || calls != 0 {
		t.Fatal("login ran without durable epoch")
	}
	if !m.changing {
		t.Fatal("uncertain account reopened")
	}
}

func TestAccountEpochInvalidCookieResultLeavesRecoverableLoggedOutState(t *testing.T) {
	store, a := epochFixture(t)
	m, _ := newAccountLeaseManager(a.AccountUserID, a.AccountEpoch, a.ProviderGeneration)
	if store.replace(context.Background(), m, func(context.Context, frozenAccount) ([]*proto.NetworkCookieParam, error) { return nil, nil }) == nil {
		t.Fatal("empty login result accepted")
	}
	reopened, err := openAccountEpochStore(store.path, a, false)
	if err != nil {
		t.Fatal("invalid callback corrupted epoch history", err)
	}
	current, path, err := reopened.current()
	if err != nil || current.AccountEpoch != 2 || path != "" {
		t.Fatal("not safely logged out", err)
	}
}
func TestAccountEpochRejectsWrongManagerBeforeLogin(t *testing.T) {
	store, a := epochFixture(t)
	m, _ := newAccountLeaseManager("other-account", a.AccountEpoch, a.ProviderGeneration)
	calls := 0
	if store.replace(context.Background(), m, func(context.Context, frozenAccount) ([]*proto.NetworkCookieParam, error) {
		calls++
		return []*proto.NetworkCookieParam{{Name: "synthetic", Value: "test", Domain: ".xiaohongshu.com", Path: "/"}}, nil
	}) == nil || calls != 0 {
		t.Fatal("wrong account reached login")
	}
}

func TestAccountEpochIndependentHandlesReserveOnce(t *testing.T) {
	store, a := epochFixture(t)
	other, err := openAccountEpochStore(store.path, a, false)
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	start := make(chan struct{})
	done := make(chan struct{}, 2)
	for _, s := range []*accountEpochStore{store, other} {
		go func(s *accountEpochStore) {
			defer func() { done <- struct{}{} }()
			<-start
			m, _ := newAccountLeaseManager(a.AccountUserID, a.AccountEpoch, a.ProviderGeneration)
			_ = s.replace(context.Background(), m, func(context.Context, frozenAccount) ([]*proto.NetworkCookieParam, error) {
				calls.Add(1)
				return []*proto.NetworkCookieParam{{Name: "synthetic", Value: "test", Domain: ".xiaohongshu.com", Path: "/"}}, nil
			})
		}(s)
	}
	close(start)
	<-done
	<-done
	if calls.Load() != 1 {
		t.Fatal("epoch allocated more than once", calls.Load())
	}
	current, _, err := store.current()
	if err != nil || current.AccountEpoch != 2 {
		t.Fatal("bad persisted epoch", err)
	}
}
func TestAccountEpochDirectorySyncFailurePreventsLogin(t *testing.T) {
	store, a := epochFixture(t)
	m, _ := newAccountLeaseManager(a.AccountUserID, a.AccountEpoch, a.ProviderGeneration)
	store.syncDirectory = func(*os.File) error { return errors.New("injected-directory-sync") }
	calls := 0
	if store.replace(context.Background(), m, func(context.Context, frozenAccount) ([]*proto.NetworkCookieParam, error) { calls++; return nil, nil }) == nil || calls != 0 {
		t.Fatal("login before durable directory")
	}
}
func TestAccountEpochMissingOrCorruptStoreCannotReinitialize(t *testing.T) {
	for _, kind := range []string{"missing", "corrupt", "gap", "wrong-generation"} {
		t.Run(kind, func(t *testing.T) {
			store, a := epochFixture(t)
			switch kind {
			case "missing":
				if err := os.RemoveAll(store.path); err != nil {
					t.Fatal(err)
				}
			case "corrupt":
				if err := os.WriteFile(filepath.Join(store.path, epochName(2)), []byte("partial"), 0600); err != nil {
					t.Fatal(err)
				}
			case "gap":
				next := a
				next.AccountEpoch = 3
				raw, _ := json.Marshal(next)
				if err := os.WriteFile(filepath.Join(store.path, epochName(3)), raw, 0600); err != nil {
					t.Fatal(err)
				}
			case "wrong-generation":
				a.ProviderGeneration = "other-generation"
			}
			if _, err := openAccountEpochStore(store.path, a, false); err == nil {
				t.Fatal("accepted lost/corrupt state")
			}
		})
	}
}
