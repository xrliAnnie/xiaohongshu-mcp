package main

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
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

func TestGuardedLoginExplicitRetryAuditedAndCapped(t *testing.T) {
	s, _, _, writes := guardedServiceFixture(t)
	epochs, base := epochFixture(t)
	s.config.Epochs = epochs
	s.manager, _ = newAccountLeaseManager(base.AccountUserID, base.AccountEpoch, base.ProviderGeneration)
	var opens atomic.Int32
	s.openLogin = func(context.Context) (privateLoginSession, error) {
		count := opens.Add(1)
		if count > 1 {
			if _, err := os.Stat(filepath.Join(epochs.path, loginRetryName(base.AccountEpoch+int64(count)))); err != nil {
				t.Error("login opened before durable retry audit", err)
			}
		}
		return &loginSessionFixture{mode: "wait", id: base.AccountUserID, image: qrFixture(t, 32)}, nil
	}
	for attempt := 1; attempt <= 3; attempt++ {
		_, _ = s.loginQR(context.Background())
		select {
		case <-s.login.done:
		case <-time.After(5 * time.Second):
			t.Fatal("login did not end")
		}
		if opens.Load() != int32(attempt) {
			t.Fatalf("explicit retry %d did not open new login", attempt)
		}
	}
	if _, err := s.loginQR(context.Background()); err != errLoginRetryLimit {
		t.Fatal("fourth failed login was not capped", err)
	}
	_, client := privateProviderFixture(t, uint32(os.Geteuid()), guardedRoutes(s, uint32(os.Geteuid())))
	response, routeErr := client.Post("http://private/v1/read/get_login_qrcode", "application/json", strings.NewReader(`{}`))
	if routeErr != nil {
		t.Fatal(routeErr)
	}
	var denied map[string]string
	decodeErr := json.NewDecoder(response.Body).Decode(&denied)
	response.Body.Close()
	if decodeErr != nil || response.StatusCode != 429 || denied["code"] != "login_retry_limit" {
		t.Fatal("retry limit lacked fixed private denial")
	}
	if opens.Load() != 3 || writes.Load() != 0 {
		t.Fatal("retry cap or write boundary violated")
	}
	current, cookie, err := epochs.current()
	if err != nil || cookie != "" || current.AccountEpoch != base.AccountEpoch+3 {
		t.Fatal("retry rewound epoch or persisted cookies", err)
	}
	config := s.config
	config.Execution.Key = append([]byte(nil), s.config.Execution.Key...)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := newGuardedService(context.Background(), config)
	if err != nil {
		t.Fatal("durable restart failed", err)
	}
	defer restarted.Close()
	restarted.openLogin = func(context.Context) (privateLoginSession, error) {
		opens.Add(1)
		return &loginSessionFixture{mode: "wait", id: base.AccountUserID, image: qrFixture(t, 32)}, nil
	}
	_, _ = restarted.loginQR(context.Background())
	<-restarted.login.done
	if opens.Load() != 4 {
		t.Fatal("restart did not clear retry counter")
	}
	after, _, err := epochs.current()
	if err != nil || after.AccountEpoch != current.AccountEpoch+1 {
		t.Fatal("restart reset durable epoch", err)
	}
}
func TestGuardedLoginRetryRequiresCleanupAndMatchingDurableEpoch(t *testing.T) {
	for _, mode := range []string{"cleanup", "epoch"} {
		t.Run(mode, func(t *testing.T) {
			s, _, _, _ := guardedServiceFixture(t)
			epochs, base := epochFixture(t)
			s.config.Epochs = epochs
			s.manager, _ = newAccountLeaseManager(base.AccountUserID, base.AccountEpoch, base.ProviderGeneration)
			var opens atomic.Int32
			s.openLogin = func(context.Context) (privateLoginSession, error) {
				opens.Add(1)
				return &loginSessionFixture{mode: map[string]string{"cleanup": "cleanup", "epoch": "wait"}[mode], id: "wrong-account", image: qrFixture(t, 32)}, nil
			}
			_, _ = s.loginQR(context.Background())
			<-s.login.done
			if mode == "epoch" {
				s.manager.mu.Lock()
				s.manager.epoch++
				s.manager.mu.Unlock()
			}
			if _, err := s.loginQR(context.Background()); err == nil {
				t.Fatal("unsafe recovery accepted")
			}
			if opens.Load() != 1 {
				t.Fatal("unsafe retry opened browser")
			}
		})
	}
}

func TestGuardedLoginSuccessClearsConsecutiveFailures(t *testing.T) {
	s, _, _, _ := guardedServiceFixture(t)
	epochs, base := epochFixture(t)
	s.config.Epochs = epochs
	s.manager, _ = newAccountLeaseManager(base.AccountUserID, base.AccountEpoch, base.ProviderGeneration)
	var opens atomic.Int32
	s.openLogin = func(context.Context) (privateLoginSession, error) {
		mode := ""
		if opens.Add(1) == 1 {
			mode = "wait"
		}
		return &loginSessionFixture{mode: mode, id: base.AccountUserID, image: qrFixture(t, 32)}, nil
	}
	_, _ = s.loginQR(context.Background())
	<-s.login.done
	_, _ = s.loginQR(context.Background())
	<-s.login.done
	if s.failedLogins != 1 {
		t.Fatal("failed login was not counted")
	}
	if _, err := s.loginQR(context.Background()); err != nil {
		t.Fatal("successful login status failed", err)
	}
	if s.failedLogins != 0 || opens.Load() != 2 {
		t.Fatal("success did not reset failure counter")
	}
}
