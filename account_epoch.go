package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/go-rod/rod/lib/proto"
	"golang.org/x/sys/unix"
)

// An append-only epoch reservation is durable before login/cookie mutation.
// A reservation with no completed cookie file means logged out, never fallback.
// Runtime opens an existing store; only explicit provisioning creates the base.
type accountEpochStore struct {
	path                    string
	base                    frozenAccount
	root                    os.FileInfo
	syncFile, syncDirectory func(*os.File) error
}

func openAccountEpochStore(path string, base frozenAccount, provision bool) (*accountEpochStore, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || !frozenID(base.AccountUserID) || !frozenID(base.ProviderInstanceID) || !frozenID(base.ProviderGeneration) || base.AccountEpoch < 1 || base.AccountEpoch > maxPermitInteger {
		return nil, errControlledCookies
	}
	if provision {
		if os.Mkdir(path, 0700) != nil {
			return nil, errControlledCookies
		}
	}
	info, err := os.Lstat(path)
	if err != nil || !privateJournalEntry(info, true) {
		return nil, errControlledCookies
	}
	s := &accountEpochStore{path: path, base: base, root: info, syncFile: (*os.File).Sync, syncDirectory: (*os.File).Sync}
	dir, err := s.directory()
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	if provision {
		raw, _ := json.Marshal(base)
		if s.write(dir, ".base", raw) != nil {
			return nil, errControlledCookies
		}
	}
	if _, _, err = s.readState(dir); err != nil {
		return nil, err
	}
	return s, nil
}
func (s *accountEpochStore) directory() (*os.File, error) {
	fd, err := unix.Open(s.path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, errControlledCookies
	}
	dir := os.NewFile(uintptr(fd), "account-epochs")
	info, err := dir.Stat()
	if err != nil || !privateJournalEntry(info, true) || !os.SameFile(info, s.root) {
		dir.Close()
		return nil, errControlledCookies
	}
	if unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB) != nil {
		dir.Close()
		return nil, errControlledCookies
	}
	return dir, nil
}
func (s *accountEpochStore) write(dir *os.File, name string, raw []byte) error {
	f, err := createJournalFile(dir, name)
	if err != nil {
		return errControlledCookies
	}
	n, writeErr := f.Write(raw)
	syncErr := s.syncFile(f)
	closeErr := f.Close()
	if writeErr != nil || n != len(raw) || syncErr != nil || closeErr != nil || s.syncDirectory(dir) != nil {
		return errControlledCookies
	}
	current, err := os.Lstat(s.path)
	if err != nil || !privateJournalEntry(current, true) || !os.SameFile(s.root, current) {
		return errControlledCookies
	}
	return nil
}
func epochName(epoch int64) string       { return fmt.Sprintf("epoch-%016d.json", epoch) }
func epochCookieName(epoch int64) string { return fmt.Sprintf("cookies-%016d.json", epoch) }
func (s *accountEpochStore) readState(dir *os.File) (frozenAccount, string, error) {
	fail := func() (frozenAccount, string, error) { return frozenAccount{}, "", errControlledCookies }
	raw, err := readJournalFile(dir, ".base")
	expected, _ := json.Marshal(s.base)
	if err != nil || !bytes.Equal(raw, expected) {
		return fail()
	}
	entries, err := dir.ReadDir(1025)
	if err != nil || len(entries) > 1024 {
		return fail()
	}
	epochs := []int64{}
	cookies := map[int64]bool{}
	retries := []int64{}
	for _, entry := range entries {
		name := entry.Name()
		if name == ".base" {
			continue
		}

		if strings.HasPrefix(name, "login-retry-") {
			number, err := strconv.ParseInt(strings.TrimSuffix(strings.TrimPrefix(name, "login-retry-"), ".json"), 10, 64)
			if err != nil || number <= s.base.AccountEpoch+1 || number > maxPermitInteger || name != loginRetryName(number) {
				return fail()
			}
			raw, err := readJournalFile(dir, name)
			expected, _ := json.Marshal(loginRetryAudit{OldEpoch: number - 1, NewEpoch: number, Cause: "explicit_login_retry"})
			if err != nil || !bytes.Equal(raw, expected) {
				return fail()
			}
			retries = append(retries, number)
			continue
		}
		prefix := "epoch-"
		cookie := strings.HasPrefix(name, "cookies-")
		if cookie {
			prefix = "cookies-"
		}
		if !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, ".json") {
			return fail()
		}
		number, err := strconv.ParseInt(strings.TrimSuffix(strings.TrimPrefix(name, prefix), ".json"), 10, 64)
		if err != nil || number <= s.base.AccountEpoch || number > maxPermitInteger {
			return fail()
		}
		a := s.base
		a.AccountEpoch = number
		if cookie {
			if name != epochCookieName(number) {
				return fail()
			}
			if _, err := loadControlledCookies(filepath.Join(s.path, name), a); err != nil {
				return fail()
			}
			cookies[number] = true
		} else {
			if name != epochName(number) {
				return fail()
			}
			raw, err := readJournalFile(dir, name)
			expected, _ := json.Marshal(a)
			if err != nil || !bytes.Equal(raw, expected) {
				return fail()
			}
			epochs = append(epochs, number)
		}
	}
	sort.Slice(epochs, func(i, j int) bool { return epochs[i] < epochs[j] })
	current := s.base
	for _, epoch := range epochs {
		if epoch != current.AccountEpoch+1 {
			return fail()
		}
		current.AccountEpoch = epoch
	}
	for epoch := range cookies {
		if epoch > current.AccountEpoch {
			return fail()
		}
	}
	for _, next := range retries {
		if next > current.AccountEpoch+1 || cookies[next-1] {
			return fail()
		}
	}
	path := ""
	if cookies[current.AccountEpoch] {
		path = filepath.Join(s.path, epochCookieName(current.AccountEpoch))
	}
	return current, path, nil
}
func (s *accountEpochStore) current() (frozenAccount, string, error) {
	dir, err := s.directory()
	if err != nil {
		return frozenAccount{}, "", err
	}
	defer dir.Close()
	return s.readState(dir)
}
func (s *accountEpochStore) reserve(next int64) (frozenAccount, error) {
	dir, err := s.directory()
	if err != nil {
		return frozenAccount{}, err
	}
	defer dir.Close()
	current, _, err := s.readState(dir)
	if err != nil || next != current.AccountEpoch+1 || next > maxPermitInteger {
		return frozenAccount{}, errControlledCookies
	}
	// Leave space for the associated cookie record; never discard epoch history.
	entries, err := os.ReadDir(s.path)
	if err != nil || len(entries) > 1021 {
		return frozenAccount{}, errControlledCookies
	}
	current.AccountEpoch = next
	raw, _ := json.Marshal(current)
	if s.write(dir, epochName(next), raw) != nil {
		return frozenAccount{}, errControlledCookies
	}
	return current, nil
}
func (s *accountEpochStore) finish(account frozenAccount, cookies []*proto.NetworkCookieParam) error {
	dir, err := s.directory()
	if err != nil {
		return err
	}
	defer dir.Close()
	current, path, err := s.readState(dir)
	if err != nil || current != account || path != "" {
		return errControlledCookies
	}
	raw, err := json.Marshal(controlledCookieRecord{SchemaVersion: 1, Account: account, Cookies: cookies})
	if err != nil || len(raw) > 1024*1024 {
		return errControlledCookies
	}
	if _, err = decodeControlledCookies(raw, account); err != nil {
		return err
	}
	if err = s.write(dir, epochCookieName(account.AccountEpoch), raw); err != nil {
		return err
	}
	_, err = loadControlledCookies(filepath.Join(s.path, epochCookieName(account.AccountEpoch)), account)
	return err
}

// obtain is the trusted provider login owner, never an RPC/model callback. It
// must establish self identity; the later write factory verifies that identity again.
func (s *accountEpochStore) replace(ctx context.Context, m *accountLeaseManager, obtain func(context.Context, frozenAccount) ([]*proto.NetworkCookieParam, error)) error {
	if m == nil || obtain == nil {
		return errControlledCookies
	}
	m.mu.Lock()
	matching := m.accountID == s.base.AccountUserID && m.generation == s.base.ProviderGeneration
	m.mu.Unlock()
	if !matching {
		return errControlledCookies
	}
	return m.replaceSession(ctx, func(ctx context.Context, next int64) error {
		account, err := s.reserve(next)
		if err != nil || ctx.Err() != nil {
			return errControlledCookies
		}
		cookies, err := obtain(ctx, account)
		if err != nil || ctx.Err() != nil {
			return errControlledCookies
		}
		return s.finish(account, cookies)
	})
}

// Audit precedes the next reservation and contains no cookies or credentials.
// A crash may leave one audit for current+1; restart retains it and does not
// pretend a new epoch or completed login already exists.
type loginRetryAudit struct {
	OldEpoch int64  `json:"oldEpoch"`
	NewEpoch int64  `json:"newEpoch"`
	Cause    string `json:"cause"`
}

func loginRetryName(next int64) string { return fmt.Sprintf("login-retry-%016d.json", next) }
func (s *accountEpochStore) recordExplicitLoginRetry(expected frozenAccount) error {
	dir, err := s.directory()
	if err != nil {
		return errControlledCookies
	}
	defer dir.Close()
	current, cookie, err := s.readState(dir)
	if err != nil || current != expected || cookie != "" || current.AccountEpoch <= s.base.AccountEpoch || current.AccountEpoch >= maxPermitInteger {
		return errControlledCookies
	}
	entries, err := os.ReadDir(s.path)
	if err != nil || len(entries) > 1020 {
		return errControlledCookies
	}
	raw, _ := json.Marshal(loginRetryAudit{OldEpoch: current.AccountEpoch, NewEpoch: current.AccountEpoch + 1, Cause: "explicit_login_retry"})
	return s.write(dir, loginRetryName(current.AccountEpoch+1), raw)
}
