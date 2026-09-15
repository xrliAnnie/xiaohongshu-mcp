package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"time"
)

type guardedAccountStatus struct {
	Account  frozenAccount  `json:"account"`
	Upstream frozenUpstream `json:"upstream"`
	LoggedIn bool           `json:"loggedIn"`
}

// Private read: uses the same account exclusion as writes, proves self identity
// on the controlled page, and confirms exact session cleanup before returning.
// No configured username or caller-provided identity is a login proof.
func (s *guardedService) accountStatus(ctx context.Context) (result guardedAccountStatus, err error) {
	if !s.mu.TryLock() {
		return result, errAccountBusy
	}
	defer s.mu.Unlock()
	if ctx.Err() != nil || s.ctx.Err() != nil {
		return result, errPrivateProvider
	}
	s.manager.mu.Lock()
	busy := s.manager.active != nil || s.manager.changing
	s.manager.mu.Unlock()
	if busy && s.login == nil {
		return result, errAccountBusy
	}
	account, cookiePath, err := s.config.Epochs.current()
	if err != nil {
		return result, errAccountMismatch
	}
	result = guardedAccountStatus{Account: account, Upstream: s.config.Upstream, LoggedIn: false}
	if cookiePath == "" {
		return result, nil
	}
	lifetime, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	stop := context.AfterFunc(s.ctx, cancel)
	defer stop()
	sum := sha256.Sum256([]byte("flywheel:xhs-account-status:v1"))
	lease, err := s.manager.prepare(lifetime, hex.EncodeToString(sum[:]), 30*time.Second, func(c context.Context) (accountLeaseSession, error) { return s.open(c, cookiePath, account) })
	if err != nil {
		return guardedAccountStatus{}, err
	}
	defer func() {
		if closeErr := lease.Close(); closeErr != nil {
			result = guardedAccountStatus{}
			err = errAccountMismatch
		}
	}()
	current, currentPath, err := s.config.Epochs.current()
	if err != nil || current != account || currentPath != cookiePath || lifetime.Err() != nil {
		return guardedAccountStatus{}, errAccountMismatch
	}
	result.LoggedIn = true
	return result, nil
}
