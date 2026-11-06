package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"testing"
)

type privateFeedsSession struct {
	*leaseSessionFixture
	reads atomic.Int32
	read  func(context.Context) (*FeedsListResponse, error)
}

func (s *privateFeedsSession) readFeeds(ctx context.Context) (*FeedsListResponse, error) {
	s.reads.Add(1)
	if s.read != nil {
		return s.read(ctx)
	}
	return &FeedsListResponse{Count: 0}, nil
}

func TestPrivateFeedsReadUsesAccountLeaseAndClosesBeforeReply(t *testing.T) {
	s, _, _, writes := guardedServiceFixture(t)
	account, _, _ := s.config.Epochs.current()
	session := &privateFeedsSession{leaseSessionFixture: &leaseSessionFixture{id: account.AccountUserID}}
	s.open = func(context.Context, string, frozenAccount) (accountLeaseSession, error) { return session, nil }
	_, client := privateProviderFixture(t, uint32(os.Geteuid()), guardedRoutes(s, uint32(os.Geteuid())))
	response, err := client.Post("http://private/v1/read/list_feeds", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var result struct {
		Account  frozenAccount     `json:"account"`
		Upstream frozenUpstream    `json:"upstream"`
		Data     FeedsListResponse `json:"data"`
	}
	if response.StatusCode != http.StatusOK || json.NewDecoder(response.Body).Decode(&result) != nil || result.Account != account || result.Upstream != s.config.Upstream {
		t.Fatal("private feed read unavailable or unbound", response.StatusCode)
	}
	if session.reads.Load() != 1 || session.closes.Load() != 1 || writes.Load() != 0 {
		t.Fatal("read ownership or zero-write invariant failed")
	}
}

func TestPrivateFeedsReadRejectsUnverifiedOrUnclosedSession(t *testing.T) {
	for _, mode := range []string{"wrong-account", "changed-account", "cleanup", "cancel", "read-error"} {
		t.Run(mode, func(t *testing.T) {
			s, _, _, writes := guardedServiceFixture(t)
			account, _, _ := s.config.Epochs.current()
			session := &privateFeedsSession{leaseSessionFixture: &leaseSessionFixture{id: account.AccountUserID}}
			switch mode {
			case "wrong-account":
				session.id = "other"
			case "changed-account":
				session.read = func(context.Context) (*FeedsListResponse, error) {
					session.id = "other"
					return &FeedsListResponse{}, nil
				}
			case "cleanup":
				session.closeErr = errors.New("synthetic-private-error")
			case "cancel":
				session.read = func(ctx context.Context) (*FeedsListResponse, error) { s.cancel(); return &FeedsListResponse{}, nil }
			case "read-error":
				session.read = func(context.Context) (*FeedsListResponse, error) { return nil, errors.New("synthetic-private-error") }
			}
			s.open = func(context.Context, string, frozenAccount) (accountLeaseSession, error) { return session, nil }
			_, client := privateProviderFixture(t, uint32(os.Geteuid()), guardedRoutes(s, uint32(os.Geteuid())))
			response, err := client.Post("http://private/v1/read/list_feeds", "application/json", strings.NewReader(`{}`))
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			var result map[string]string
			if response.StatusCode != 503 || json.NewDecoder(response.Body).Decode(&result) != nil || result["code"] != "private_provider_unavailable" {
				t.Fatal("unverified read or unsafe failure", response.StatusCode)
			}
			if writes.Load() != 0 {
				t.Fatal("read mutated")
			}
			if session.closes.Load() != 1 || (mode == "wrong-account" && session.reads.Load() != 0) {
				t.Fatal("unproven session used or owner not closed")
			}
			if mode == "cleanup" {
				s.manager.mu.Lock()
				retained := s.manager.active != nil
				s.manager.mu.Unlock()
				if !retained {
					t.Fatal("failed cleanup released account exclusion")
				}
			}
		})
	}
}

func TestPrivateFeedsReadPreservesPreparedWriteAndValidatesInput(t *testing.T) {
	s, raw, digest, writes := guardedServiceFixture(t)
	if _, err := s.prepare(context.Background(), raw, digest, nil); err != nil {
		t.Fatal(err)
	}
	_, client := privateProviderFixture(t, uint32(os.Geteuid()), guardedRoutes(s, uint32(os.Geteuid())))
	for _, test := range []struct {
		body   string
		status int
	}{{`{}`, 409}, {`{"xsec_token":"synthetic-secret"}`, 400}, {`{"accountUserId":"other"}`, 400}} {
		response, err := client.Post("http://private/v1/read/list_feeds", "application/json", strings.NewReader(test.body))
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != test.status {
			t.Fatal("account exclusion/input validation", response.StatusCode, test.status)
		}
	}
	if writes.Load() != 0 {
		t.Fatal("read dispatched prepared write")
	}
}
