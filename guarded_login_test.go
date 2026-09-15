package main

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type waitingLogin struct {
	*loginSessionFixture
	release chan struct{}
}

func (s *waitingLogin) wait(ctx context.Context) bool {
	select {
	case <-s.release:
		return true
	case <-ctx.Done():
		return false
	}
}
func TestGuardedLoginReusesQRAndCommitsOnlyAfterScan(t *testing.T) {
	s, _, _, writes := guardedServiceFixture(t)
	epochs, a := epochFixture(t)
	s.config.Epochs = epochs
	s.manager, _ = newAccountLeaseManager(a.AccountUserID, a.AccountEpoch, a.ProviderGeneration)
	owner := &waitingLogin{loginSessionFixture: &loginSessionFixture{id: a.AccountUserID, image: qrFixture(t, 32)}, release: make(chan struct{})}
	var opens atomic.Int32
	s.openLogin = func(context.Context) (privateLoginSession, error) { opens.Add(1); return owner, nil }
	ctx, cancel := context.WithTimeout(context.Background(), time.Second*5)
	defer cancel()
	first, err := s.loginQR(ctx)
	if err != nil || first.Image == "" || first.LoggedIn {
		t.Fatal("QR unavailable", err)
	}
	second, err := s.loginQR(ctx)
	if err != nil || second != first || opens.Load() != 1 {
		t.Fatal("QR request replaced active login")
	}
	status, err := s.accountStatus(ctx)
	if err != nil || status.LoggedIn || status.Account.AccountEpoch != a.AccountEpoch+1 {
		t.Fatal("pending login became proof", err)
	}
	close(owner.release)
	select {
	case <-s.login.done:
	case <-ctx.Done():
		t.Fatal("login did not finish")
	}
	_, path, err := epochs.current()
	if err != nil || path == "" || owner.closed != 1 || writes.Load() != 0 {
		t.Fatal("login close/persistence invariant failed")
	}
}
func TestGuardedLoginCloseCancelsAndRetainsFailedOwner(t *testing.T) {
	s, _, _, _ := guardedServiceFixture(t)
	epochs, a := epochFixture(t)
	s.config.Epochs = epochs
	s.manager, _ = newAccountLeaseManager(a.AccountUserID, a.AccountEpoch, a.ProviderGeneration)
	owner := &waitingLogin{loginSessionFixture: &loginSessionFixture{mode: "cleanup", id: a.AccountUserID, image: qrFixture(t, 32)}, release: make(chan struct{})}
	s.openLogin = func(context.Context) (privateLoginSession, error) { return owner, nil }
	if _, err := s.loginQR(context.Background()); err != nil {
		t.Fatal(err)
	}
	if s.Close() == nil {
		t.Fatal("unconfirmed login cleanup accepted")
	}
	_, path, err := epochs.current()
	if err != nil || path != "" || s.login == nil || s.login.owner == nil {
		t.Fatal("failed login owner/cookie state lost")
	}
}

func TestGuardedLoginPrivateRouteRejectsExtraInputAndPreparedWrite(t *testing.T) {
	s, raw, digest, writes := guardedServiceFixture(t)
	if _, err := s.prepare(context.Background(), raw, digest, nil); err != nil {
		t.Fatal(err)
	}
	_, client := privateProviderFixture(t, uint32(os.Geteuid()), guardedRoutes(s, uint32(os.Geteuid())))
	for _, test := range []struct {
		body   string
		status int
	}{{`{}`, 409}, {`{"cookie":"forged"}`, 400}} {
		response, err := client.Post("http://private/v1/read/get_login_qrcode", "application/json", strings.NewReader(test.body))
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != test.status {
			t.Fatal("login bypassed private input/account exclusion")
		}
	}
	if writes.Load() != 0 || s.login != nil {
		t.Fatal("login changed prepared write")
	}
}
func TestGuardedLoginPrivateRouteReturnsOnlyScopedQR(t *testing.T) {
	s, _, _, _ := guardedServiceFixture(t)
	epochs, a := epochFixture(t)
	s.config.Epochs = epochs
	s.manager, _ = newAccountLeaseManager(a.AccountUserID, a.AccountEpoch, a.ProviderGeneration)
	owner := &waitingLogin{loginSessionFixture: &loginSessionFixture{id: a.AccountUserID, image: qrFixture(t, 32)}, release: make(chan struct{})}
	s.openLogin = func(context.Context) (privateLoginSession, error) { return owner, nil }
	_, client := privateProviderFixture(t, uint32(os.Geteuid()), guardedRoutes(s, uint32(os.Geteuid())))
	response, err := client.Post("http://private/v1/read/get_login_qrcode", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var result map[string]json.RawMessage
	if response.StatusCode != http.StatusOK || json.NewDecoder(response.Body).Decode(&result) != nil || len(result) != 5 || result["image"] == nil || result["account"] == nil {
		t.Fatal("unsafe QR response")
	}
}

func TestGuardedLoginDisconnectBeforeQRStopsOwnerStartup(t *testing.T) {
	s, _, _, _ := guardedServiceFixture(t)
	epochs, a := epochFixture(t)
	s.config.Epochs = epochs
	s.manager, _ = newAccountLeaseManager(a.AccountUserID, a.AccountEpoch, a.ProviderGeneration)
	entered := make(chan struct{})
	s.openLogin = func(ctx context.Context) (privateLoginSession, error) {
		close(entered)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	returned := make(chan error, 1)
	go func() { _, err := s.loginQR(ctx); returned <- err }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("login startup missing")
	}
	cancel()
	select {
	case err := <-returned:
		if err == nil {
			t.Fatal("cancelled request succeeded")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancelled request stuck")
	}
	select {
	case <-s.login.done:
	case <-time.After(5 * time.Second):
		t.Fatal("login owner lifetime not cancelled")
	}
	_, path, err := epochs.current()
	if err != nil || path != "" {
		t.Fatal("cancelled login persisted cookies")
	}
}
