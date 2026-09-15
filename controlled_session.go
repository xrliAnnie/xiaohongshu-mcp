package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/proto"
	"github.com/xpzouying/xiaohongshu-mcp/browser"
	"golang.org/x/sys/unix"
)

var errControlledCookies = errors.New("controlled_session_invalid")

type controlledCookieRecord struct {
	SchemaVersion int                         `json:"schemaVersion"`
	Account       frozenAccount               `json:"account"`
	Cookies       []*proto.NetworkCookieParam `json:"cookies"`
	digest        [32]byte
}

// path is private startup policy, never an RPC/model parameter. The startup
// loader must also validate its ancestors and dedicated service UID.
func loadControlledCookies(path string, expected frozenAccount) (*controlledCookieRecord, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, errControlledCookies
	}
	root := filepath.Dir(path)
	before, err := os.Lstat(root)
	if err != nil || !privateJournalEntry(before, true) {
		return nil, errControlledCookies
	}
	fd, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, errControlledCookies
	}
	dir := os.NewFile(uintptr(fd), "controlled-cookie-directory")
	defer dir.Close()
	info, err := dir.Stat()
	if err != nil || !os.SameFile(before, info) {
		return nil, errControlledCookies
	}
	cookieFD, err := unix.Openat(fd, filepath.Base(path), unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, errControlledCookies
	}
	file := os.NewFile(uintptr(cookieFD), "controlled-cookies")
	defer file.Close()
	start, err := file.Stat()
	if err != nil || !providerMediaFile(start, start.Size()) || start.Size() < 1 || start.Size() > 1024*1024 {
		return nil, errControlledCookies
	}
	raw, err := io.ReadAll(io.LimitReader(file, 1024*1024+1))
	if err != nil || int64(len(raw)) != start.Size() {
		return nil, errControlledCookies
	}
	end, err := file.Stat()
	current, pathErr := os.Lstat(path)
	after, rootErr := os.Lstat(root)
	if err != nil || pathErr != nil || rootErr != nil || !providerMediaFile(end, start.Size()) || !os.SameFile(start, current) || !os.SameFile(before, after) || !start.ModTime().Equal(end.ModTime()) {
		return nil, errControlledCookies
	}
	unique := json.NewDecoder(bytes.NewReader(raw))
	if _, err = uniqueRPCValue(unique, 0); err != nil {
		return nil, errControlledCookies
	}
	if _, err = unique.Token(); err != io.EOF {
		return nil, errControlledCookies
	}
	var record controlledCookieRecord
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&record) != nil || record.SchemaVersion != 1 || record.Account != expected || !frozenID(expected.AccountUserID) || !frozenID(expected.ProviderInstanceID) || !frozenID(expected.ProviderGeneration) || expected.AccountEpoch < 1 || expected.AccountEpoch > maxPermitInteger || len(record.Cookies) < 1 || len(record.Cookies) > 256 {
		return nil, errControlledCookies
	}
	seen := map[string]bool{}
	for _, c := range record.Cookies {
		if c == nil || c.Name == "" || len(c.Name) > 256 || len(c.Value) > 16384 || c.URL != "" || c.PartitionKey != nil || !strings.HasPrefix(c.Path, "/") || strings.ContainsAny(c.Name+c.Value+c.Domain+c.Path, "\x00\r\n") {
			return nil, errControlledCookies
		}
		domain := strings.TrimPrefix(c.Domain, ".")
		if domain != "xiaohongshu.com" && !strings.HasSuffix(domain, ".xiaohongshu.com") {
			return nil, errControlledCookies
		}
		key := c.Domain + "\x00" + c.Path + "\x00" + c.Name
		if seen[key] {
			return nil, errControlledCookies
		}
		seen[key] = true
	}
	record.digest = sha256.Sum256(raw)
	return &record, nil
}

type controlledSession struct {
	owned        *browser.PipeBrowser
	page         *rod.Page
	cookiePath   string
	account      frozenAccount
	cookieDigest [32]byte
	once         sync.Once
	closeErr     error
}

var _ guardedWriteSession = (*controlledSession)(nil)

// Production factory: a fresh private profile, anonymous CDP pipes, explicitly
// loaded private cookies, and one page for self identity and all write actions.
func openControlledSession(ctx context.Context, options browser.PipeBrowserOptions, cookiePath string, account frozenAccount) (accountLeaseSession, error) {
	if ctx.Err() != nil {
		return nil, errControlledCookies
	}
	record, err := loadControlledCookies(cookiePath, account)
	if err != nil {
		return nil, err
	}
	owned, err := browser.LaunchPipeBrowser(ctx, options)
	if err != nil {
		if owned != nil {
			return &controlledSession{owned: owned}, errControlledCookies
		}
		return nil, errControlledCookies
	}
	session := &controlledSession{owned: owned, cookiePath: cookiePath, account: account, cookieDigest: record.digest}
	// Return the owner even on failure. accountLeaseManager must retain exclusion
	// when exact child/profile cleanup cannot be confirmed.
	if owned.Browser.Context(ctx).SetCookies(record.Cookies) != nil {
		return session, errControlledCookies
	}
	page, err := owned.Browser.Context(ctx).Page(proto.TargetCreateTarget{URL: "about:blank"})
	if err != nil {
		return session, errControlledCookies
	}
	session.page = page
	if _, err = session.selfAccount(ctx); err != nil {
		return session, errControlledCookies
	}
	return session, nil
}
func (s *controlledSession) writePage() *rod.Page { return s.page }
func (s *controlledSession) selfAccount(ctx context.Context) (string, error) {
	if s.page == nil || ctx.Err() != nil {
		return "", errControlledCookies
	}
	record, err := loadControlledCookies(s.cookiePath, s.account)
	if err != nil || record.digest != s.cookieDigest {
		return "", errControlledCookies
	}
	id, err := readSelfAccount(ctx, rodSelfAccountPage{page: s.page})
	if err != nil || id != s.account.AccountUserID {
		return "", errAccountMismatch
	}
	return id, nil
}
func (s *controlledSession) Close() error {
	s.once.Do(func() { s.closeErr = s.owned.Close() })
	return s.closeErr
}
