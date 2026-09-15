package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"

	"github.com/xpzouying/xiaohongshu-mcp/xiaohongshu"
)

type privateFeedsReader interface {
	accountLeaseSession
	readFeeds(context.Context) (*FeedsListResponse, error)
}

// Results may contain platform tokens and are exclusively for the private
// authority to project into opaque handles. Never mount this on a model MCP.
type guardedFeedsResult struct {
	Account  frozenAccount      `json:"account"`
	Upstream frozenUpstream     `json:"upstream"`
	Data     *FeedsListResponse `json:"data"`
}

func (s *controlledSession) readFeeds(ctx context.Context) (*FeedsListResponse, error) {
	feeds, err := xiaohongshu.NewFeedsListAction(s.page.Context(ctx)).GetFeedsList(ctx)
	if err != nil {
		return nil, errPrivateProvider
	}
	return &FeedsListResponse{Feeds: feeds, Count: len(feeds)}, nil
}

func (s *guardedService) readFeeds(ctx context.Context) (result guardedFeedsResult, err error) {
	if !s.mu.TryLock() {
		return result, errAccountBusy
	}
	defer s.mu.Unlock()
	if ctx.Err() != nil || s.ctx.Err() != nil {
		return result, errPrivateProvider
	}
	account, cookiePath, err := s.config.Epochs.current()
	if err != nil || cookiePath == "" {
		return result, errAccountMismatch
	}
	lifetime, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	stop := context.AfterFunc(s.ctx, cancel)
	defer stop()
	sum := sha256.Sum256([]byte("flywheel:xhs-private-read:list-feeds:v1"))
	lease, err := s.manager.prepare(lifetime, hex.EncodeToString(sum[:]), 90*time.Second, func(c context.Context) (accountLeaseSession, error) { return s.open(c, cookiePath, account) })
	if err != nil {
		return result, err
	}
	defer func() {
		if closeErr := lease.Close(); closeErr != nil {
			result = guardedFeedsResult{}
			err = errAccountMismatch
		}
	}()
	// Teardown may interrupt a blocked browser operation, but the account slot
	// cannot be released while this read and its final identity proof are active.
	lease.operation.Lock()
	defer lease.operation.Unlock()
	reader, ok := lease.session.(privateFeedsReader)
	if !ok {
		return result, errPrivateProvider
	}
	data, err := reader.readFeeds(lease.ctx)
	if err != nil || data == nil || ctx.Err() != nil || s.ctx.Err() != nil || lease.recheckLocked(lifetime) != nil {
		return result, errPrivateProvider
	}
	current, currentPath, err := s.config.Epochs.current()
	if err != nil || current != account || currentPath != cookiePath || lifetime.Err() != nil {
		return result, errAccountMismatch
	}
	raw, err := json.Marshal(data)
	if err != nil || len(raw) > 196608 {
		return result, errPrivateProvider
	}
	return guardedFeedsResult{Account: account, Upstream: s.config.Upstream, Data: data}, nil
}
