package main

import (
	"context"
	"errors"
	"github.com/go-rod/rod/lib/proto"
	"testing"
)

type loginSessionFixture struct {
	mode, id, image     string
	closed, cookieReads int
	onClose             func()
}

func (s *loginSessionFixture) qr(context.Context) (string, bool, error) {
	if s.mode == "panic" {
		panic("PRIVATE_CANARY")
	}
	if s.mode == "bad-qr" {
		return "https://unsafe/qr", false, nil
	}
	return s.image, s.mode == "already-in", nil
}
func (s *loginSessionFixture) wait(context.Context) bool { return s.mode != "wait" }
func (s *loginSessionFixture) self(context.Context) (string, error) {
	if s.mode == "identity" {
		return "other", nil
	}
	return s.id, nil
}
func (s *loginSessionFixture) cookies(context.Context) ([]*proto.NetworkCookieParam, error) {
	s.cookieReads++
	domain := ".xiaohongshu.com"
	if s.mode == "cookies" {
		domain = ".evil.test"
	}
	return []*proto.NetworkCookieParam{{Name: "synthetic", Value: "test", Domain: domain, Path: "/"}}, nil
}
func (s *loginSessionFixture) Close() error {
	s.closed++
	if s.onClose != nil {
		s.onClose()
	}
	if s.mode == "cleanup" {
		return errors.New("PRIVATE_CANARY")
	}
	return nil
}
func TestControlledLoginPublishesOnlyQRAndPersistsAfterVerifiedClose(t *testing.T) {
	for _, mode := range []string{"success", "bad-qr", "already-in", "wait", "identity", "cookies", "cleanup", "publish", "panic", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			store, a := epochFixture(t)
			manager, _ := newAccountLeaseManager(a.AccountUserID, a.AccountEpoch, a.ProviderGeneration)
			session := &loginSessionFixture{mode: mode, id: a.AccountUserID, image: qrFixture(t, 32)}
			session.onClose = func() {
				_, path, err := store.current()
				if err != nil || path != "" {
					t.Fatal("cookies persisted before close")
				}
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			published := 0
			err := store.replace(ctx, manager, func(ctx context.Context, next frozenAccount) ([]*proto.NetworkCookieParam, error) {
				return obtainPrivateLogin(ctx, next, session, func(_ context.Context, image string) error {
					published++
					if image != session.image {
						t.Fatal("unexpected QR projection")
					}
					if mode == "cancel" {
						cancel()
					}
					if mode == "publish" {
						return errors.New("PRIVATE_CANARY")
					}
					return nil
				})
			})
			_, path, stateErr := store.current()
			if stateErr != nil || session.closed != 1 {
				t.Fatal("login owner not closed exactly once")
			}
			if mode == "success" {
				if err != nil || path == "" || published != 1 {
					t.Fatal("valid login not committed")
				}
			} else if err == nil || path != "" {
				t.Fatal("failed login committed cookies")
			}
			if (mode == "identity" || mode == "wait" || mode == "bad-qr") && session.cookieReads != 0 {
				t.Fatal("cookies captured before proof")
			}
		})
	}
}

func TestControlledLoginRejectsPartitionedCookiesBeforeConversion(t *testing.T) {
	for _, cookie := range []*proto.NetworkCookie{nil, {Name: "session", PartitionKey: &proto.NetworkCookiePartitionKey{}}, {Name: "session", PartitionKeyOpaque: true}} {
		if _, err := privateLoginCookies([]*proto.NetworkCookie{cookie}); err == nil {
			t.Fatal("unsupported cookie authority lost during conversion")
		}
	}
	result, err := privateLoginCookies([]*proto.NetworkCookie{{Name: "session", Value: "synthetic", Domain: ".xiaohongshu.com", Path: "/", Secure: true, HTTPOnly: true}})
	if err != nil || len(result) != 1 || !result[0].Secure || !result[0].HTTPOnly || result[0].Value != "synthetic" {
		t.Fatal("cookie attributes lost")
	}
}
