package main

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/proto"
	"github.com/xpzouying/xiaohongshu-mcp/browser"
	"github.com/xpzouying/xiaohongshu-mcp/xiaohongshu"
)

type privateLoginSession interface {
	qr(context.Context) (string, bool, error)
	wait(context.Context) bool
	self(context.Context) (string, error)
	cookies(context.Context) ([]*proto.NetworkCookieParam, error)
	Close() error
}

// Called only by the trusted epoch replacement owner. The callback publishes
// a normalized QR image, never cookie bytes. Cookies cannot be committed until
// self identity, cookie validation and exact browser cleanup all succeed.
func obtainPrivateLogin(ctx context.Context, account frozenAccount, owner privateLoginSession, publish func(context.Context, string) error) (cookies []*proto.NetworkCookieParam, err error) {
	if owner == nil {
		return nil, errPrivateProvider
	}
	lifetime, cancel := context.WithTimeout(ctx, 4*time.Minute)
	defer cancel()
	defer func() {
		if recover() != nil {
			cookies = nil
			err = errPrivateProvider
		}
		if closePrivateLogin(owner) != nil || lifetime.Err() != nil {
			cookies = nil
			err = errPrivateProvider
		}
	}()
	if lifetime.Err() != nil || publish == nil {
		return nil, errPrivateProvider
	}
	image, loggedIn, err := owner.qr(lifetime)
	if err != nil || loggedIn || lifetime.Err() != nil {
		return nil, errPrivateProvider
	}
	image, err = normalizePrivateLoginQR(image)
	if err != nil || publish(lifetime, image) != nil || lifetime.Err() != nil || !owner.wait(lifetime) {
		return nil, errPrivateProvider
	}
	id, err := owner.self(lifetime)
	if err != nil || id != account.AccountUserID || lifetime.Err() != nil {
		return nil, errAccountMismatch
	}
	rawCookies, err := owner.cookies(lifetime)
	if err != nil || lifetime.Err() != nil {
		return nil, errPrivateProvider
	}
	raw, err := json.Marshal(controlledCookieRecord{SchemaVersion: 1, Account: account, Cookies: rawCookies})
	if err != nil || len(raw) > 1024*1024 {
		return nil, errControlledCookies
	}
	record, err := decodeControlledCookies(raw, account)
	if err != nil {
		return nil, errControlledCookies
	}
	return record.Cookies, nil
}
func closePrivateLogin(owner privateLoginSession) (err error) {
	defer func() {
		if recover() != nil {
			err = errPrivateProvider
		}
	}()
	return owner.Close()
}

type controlledLogin struct {
	owned    *browser.PipeBrowser
	page     *rod.Page
	once     sync.Once
	closeErr error
}

// A login owner has no writePage method and receives no previous cookie file.
// Return a partial owner on startup failure so callers retain cleanup ownership.
func openControlledLogin(ctx context.Context, options browser.PipeBrowserOptions) (privateLoginSession, error) {
	if ctx.Err() != nil {
		return nil, errPrivateProvider
	}
	owned, err := browser.LaunchPipeBrowser(ctx, options)
	if owned == nil {
		return nil, errPrivateProvider
	}
	owner := &controlledLogin{owned: owned}
	if err != nil {
		return owner, errPrivateProvider
	}
	page, err := owned.Browser.Context(ctx).Page(proto.TargetCreateTarget{URL: "about:blank"})
	if err != nil {
		return owner, errPrivateProvider
	}
	owner.page = page
	return owner, nil
}
func (s *controlledLogin) qr(ctx context.Context) (string, bool, error) {
	return xiaohongshu.NewLogin(s.page).FetchQrcodeImage(ctx)
}
func (s *controlledLogin) wait(ctx context.Context) bool {
	return xiaohongshu.NewLogin(s.page).WaitForLogin(ctx)
}
func (s *controlledLogin) self(ctx context.Context) (string, error) {
	return readSelfAccount(ctx, rodSelfAccountPage{page: s.page})
}
func (s *controlledLogin) cookies(ctx context.Context) ([]*proto.NetworkCookieParam, error) {
	cookies, err := s.owned.Browser.Context(ctx).GetCookies()
	if err != nil {
		return nil, errPrivateProvider
	}
	return privateLoginCookies(cookies)
}
func (s *controlledLogin) Close() error {
	s.once.Do(func() { s.closeErr = s.owned.Close() })
	return s.closeErr
}

// rod's generic conversion drops partition information. Reject it before
// conversion rather than silently turning partitioned cookies into global ones.
func privateLoginCookies(cookies []*proto.NetworkCookie) ([]*proto.NetworkCookieParam, error) {
	if len(cookies) < 1 || len(cookies) > 256 {
		return nil, errControlledCookies
	}
	for _, cookie := range cookies {
		if cookie == nil || cookie.PartitionKey != nil || cookie.PartitionKeyOpaque {
			return nil, errControlledCookies
		}
	}
	return proto.CookiesToParams(cookies), nil
}
