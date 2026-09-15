package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/go-rod/rod"
	"github.com/xpzouying/xiaohongshu-mcp/browser"
)

type guardedServiceConfig struct {
	Epochs    *accountEpochStore
	Journal   *writeJournal
	MediaRoot string
	Browser   browser.PipeBrowserOptions
	Upstream  frozenUpstream
	Execution providerExecutionPolicy
	Decode    func(context.Context, string, string) error
	Admit     func(context.Context, internalPermit) error
	Resolve   guardedTokenResolver
}
type guardedService struct {
	config     guardedServiceConfig
	ctx        context.Context
	cancel     context.CancelFunc
	mu         sync.Mutex
	manager    *accountLeaseManager
	mediaRoot  os.FileInfo
	executions map[string]*privateExecution
	dispatcher *guardedDispatcher
	open       func(context.Context, string, frozenAccount) (accountLeaseSession, error)
}
type preparedLease struct {
	LeaseID            string `json:"leaseId"`
	AccountUserID      string `json:"accountUserId"`
	AccountEpoch       int64  `json:"accountEpoch"`
	ProviderGeneration string `json:"providerGeneration"`
	ContentDigest      string `json:"contentDigest"`
	LeaseExpiresAt     int64  `json:"leaseExpiresAt"`
}

func newGuardedService(ctx context.Context, c guardedServiceConfig) (*guardedService, error) {
	if ctx == nil || ctx.Err() != nil || c.Epochs == nil || c.Journal == nil || c.Decode == nil || c.Admit == nil || c.Resolve == nil || len(c.Execution.Key) != 32 || !frozenID(c.Execution.KeyID) || !journalDigest.MatchString(c.Upstream.BinarySHA256) || !journalDigest.MatchString(c.Upstream.ToolSchemaDigest) || c.Upstream.GuardProtocol != 1 {
		return nil, errPrivateProvider
	}
	account, _, err := c.Epochs.current()
	if err != nil || c.Execution.Audience != account.ProviderInstanceID || c.Journal.generation != account.ProviderGeneration {
		return nil, errPrivateProvider
	}
	if !filepath.IsAbs(c.MediaRoot) || filepath.Clean(c.MediaRoot) != c.MediaRoot {
		return nil, errPrivateProvider
	}
	info, err := os.Lstat(c.MediaRoot)
	if err != nil || !privateJournalEntry(info, true) {
		return nil, errPrivateProvider
	}
	manager, err := newAccountLeaseManager(account.AccountUserID, account.AccountEpoch, account.ProviderGeneration)
	if err != nil {
		return nil, err
	}
	lifetime, cancel := context.WithCancel(ctx)
	c.Execution.Key = append([]byte(nil), c.Execution.Key...)
	s := &guardedService{config: c, ctx: lifetime, cancel: cancel, manager: manager, mediaRoot: info, executions: map[string]*privateExecution{}, dispatcher: newGuardedDispatcher(c.Resolve)}
	s.open = func(ctx context.Context, path string, account frozenAccount) (accountLeaseSession, error) {
		return openControlledSession(ctx, c.Browser, path, account)
	}
	return s, nil
}
func (s *guardedService) prepare(ctx context.Context, raw []byte, digest string, nextMedia func(int) (io.Reader, error)) (preparedLease, error) {
	var result preparedLease
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
	if busy {
		return result, errAccountBusy
	}
	// Cache eviction never deletes durable consumption history.
	for id, e := range s.executions {
		if time.Now().After(e.lease.expiresAt) {
			delete(s.executions, id)
		}
	}
	if len(s.executions) >= 128 {
		return result, errAccountBusy
	}
	account, cookiePath, err := s.config.Epochs.current()
	if err != nil || cookiePath == "" {
		return result, errAccountMismatch
	}
	w, err := verifyFrozenWrite(raw, digest, time.Now().UnixMilli())
	if err != nil || w.Account != account || w.Upstream != s.config.Upstream {
		return result, errFrozenWrite
	}
	current, err := os.Lstat(s.config.MediaRoot)
	if err != nil || !privateJournalEntry(current, true) || !os.SameFile(current, s.mediaRoot) {
		return result, errProviderArtifact
	}
	entries, err := os.ReadDir(s.config.MediaRoot)
	if err != nil || len(entries) >= 8 {
		return result, errProviderArtifact
	}
	root, err := os.MkdirTemp(s.config.MediaRoot, "prepare-")
	if err != nil {
		return result, errProviderArtifact
	}
	media, err := newProviderArtifactStore(root)
	if err != nil {
		return result, err
	}
	owner := &preparedSessionOwner{media: media}
	handedOff := false
	defer func() {
		if !handedOff {
			owner.Close()
		}
	}()
	lifetime, cancel := context.WithTimeout(s.ctx, 120*time.Second)
	owner.cancel = cancel
	stop := context.AfterFunc(ctx, cancel)
	defer stop()
	keep := false
	defer func() {
		if !keep {
			cancel()
		}
	}()
	var artifacts []*providerArtifact
	for i, descriptor := range w.Media {
		if nextMedia == nil {
			return result, errProviderArtifact
		}
		input, err := nextMedia(i)
		if err != nil {
			return result, errProviderArtifact
		}
		artifact, err := media.importArtifact(lifetime, descriptor, input, s.config.Decode)
		if err != nil {
			return result, err
		}
		artifacts = append(artifacts, artifact)
	}
	if nextMedia != nil {
		if _, err := nextMedia(len(w.Media)); err != io.EOF {
			return result, errProviderArtifact
		}
	}
	p, err := newPreparedPayload(raw, digest, providerPayloadPolicy{Account: account, Upstream: s.config.Upstream}, artifacts, time.Now().UnixMilli())
	if err != nil {
		return result, err
	}
	lease, err := s.manager.prepare(lifetime, digest, 120*time.Second, func(ctx context.Context) (accountLeaseSession, error) {
		inner, err := s.open(ctx, cookiePath, account)
		owner.inner = inner
		// Even failed startup ownership belongs to the lease manager.
		handedOff = true
		return owner, err
	})
	if err != nil {
		return result, err
	}
	if ctx.Err() != nil || lifetime.Err() != nil {
		lease.Close()
		return result, errPrivateProvider
	}
	e, err := newPayloadExecution(p, lease, s.config.Journal, s.config.Execution, s.config.Admit, func(ctx context.Context, v *verifiedWrite, _ frozenWrite, _ []string) error {
		return s.dispatcher.dispatch(ctx, v)
	})
	if err != nil {
		lease.Close()
		return result, err
	}
	s.executions[lease.id] = e
	// Detach only the successful prepare request, keeping service and lease deadlines.
	if !stop() && ctx.Err() != nil {
		lease.Close()
		delete(s.executions, lease.id)
		return result, errPrivateProvider
	}
	keep = true
	return preparedLease{lease.id, lease.accountID, lease.epoch, lease.generation, lease.digest, lease.expiresAt.UnixMilli()}, nil
}
func (s *guardedService) commit(ctx context.Context, leaseID string, raw []byte, signature string) string {
	if !journalID.MatchString(leaseID) || ctx.Err() != nil || s.ctx.Err() != nil {
		return "denied"
	}
	s.mu.Lock()
	e := s.executions[leaseID]
	s.mu.Unlock()
	if e == nil {
		return "denied"
	}
	return e.commit(ctx, raw, signature)
}
func (s *guardedService) status(receipt, attempt, digest string) string {
	if !journalID.MatchString(receipt) || !journalID.MatchString(attempt) || !journalDigest.MatchString(digest) {
		return "denied"
	}
	s.mu.Lock()
	executions := make([]*privateExecution, 0, len(s.executions))
	for _, e := range s.executions {
		executions = append(executions, e)
	}
	s.mu.Unlock()
	for _, e := range executions {
		if !e.mu.TryLock() {
			continue
		}
		var p internalPermit
		match := json.Unmarshal(e.raw, &p) == nil && p.ReceiptID == receipt && p.AttemptID == attempt && p.ContentDigest == digest
		state := e.state
		e.mu.Unlock()
		if match && state != "" {
			return state
		}
	}
	dir, err := s.config.Journal.openDirectory()
	if err != nil {
		return "unknown"
	}
	defer dir.Close()
	raw, err := readJournalFile(dir, receipt+".json")
	if err != nil {
		return "unknown"
	}
	expected, _ := json.Marshal(writeTombstone{ReceiptID: receipt, AttemptID: attempt, Digest: digest, Generation: s.config.Journal.generation})
	if !bytes.Equal(raw, expected) {
		return "denied"
	}
	return "unknown"
}
func (s *guardedService) Close() error {
	s.cancel()
	s.mu.Lock()
	defer s.mu.Unlock()
	var result error
	s.manager.mu.Lock()
	active := s.manager.active
	s.manager.mu.Unlock()
	if active != nil {
		if active.Close() != nil {
			result = errPrivateProvider
		}
	}
	for _, e := range s.executions {
		if err := e.lease.Close(); err != nil {
			result = errPrivateProvider
		}
	}
	return result
}

type preparedSessionOwner struct {
	inner    accountLeaseSession
	media    *providerArtifactStore
	cancel   context.CancelFunc
	once     sync.Once
	closeErr error
}

func (o *preparedSessionOwner) selfAccount(ctx context.Context) (string, error) {
	if o.inner == nil {
		return "", errAccountMismatch
	}
	return o.inner.selfAccount(ctx)
}
func (o *preparedSessionOwner) writePage() *rod.Page {
	if inner, ok := o.inner.(guardedWriteSession); ok {
		return inner.writePage()
	}
	return nil
}
func (o *preparedSessionOwner) Close() error {
	o.once.Do(func() {
		if o.cancel != nil {
			o.cancel()
		}
		if o.inner != nil {
			if err := o.inner.Close(); err != nil {
				o.closeErr = errPrivateProvider
				return
			}
		}
		current, err := os.Lstat(o.media.root)
		if err != nil || !privateJournalEntry(current, true) || !os.SameFile(current, o.media.info) {
			o.closeErr = errProviderArtifact
			return
		}
		if os.RemoveAll(o.media.root) != nil {
			o.closeErr = errProviderArtifact
		}
	})
	return o.closeErr
}
