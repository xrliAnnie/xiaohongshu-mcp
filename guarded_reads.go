package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/xpzouying/xiaohongshu-mcp/xiaohongshu"
)

type privateOperationReader interface {
	readPrivate(context.Context, string, any) (any, error)
}

// Internal requests use fully expanded fields; optional model inputs are
// normalized by the authority. No arbitrary tool names or write dispatch.
func decodeGuardedRead(w http.ResponseWriter, r *http.Request, operation string) (any, bool) {
	var value any
	var keys []string
	switch operation {
	case "search_feeds":
		value = &SearchFeedsArgs{}
		keys = []string{"keyword", "filters", "limit"}
	case "get_feed_detail":
		value = &FeedDetailArgs{}
		keys = []string{"feed_id", "xsec_token", "load_all_comments", "limit", "click_more_replies", "reply_limit", "scroll_speed"}
	case "user_profile":
		value = &UserProfileArgs{}
		keys = []string{"user_id", "xsec_token"}
	case "list_collections":
		value = &ListCollectionsArgs{}
		keys = []string{"limit"}
	case "get_collection_content":
		value = &GetCollectionContentArgs{}
		keys = []string{"collection_id", "limit"}
	case "list_saved_content":
		value = &ListSavedContentArgs{}
		keys = []string{"limit"}
	default:
		return nil, false
	}
	r.Body = http.MaxBytesReader(w, r.Body, 65536)
	raw, err := io.ReadAll(r.Body)
	if err != nil || !utf8.Valid(raw) {
		return nil, false
	}
	unique := json.NewDecoder(bytes.NewReader(raw))
	object, err := uniqueRPCValue(unique, 0)
	if err != nil {
		return nil, false
	}
	if _, err = unique.Token(); err != io.EOF {
		return nil, false
	}
	fields, ok := object.(map[string]any)
	if !ok || len(fields) != len(keys) {
		return nil, false
	}
	for _, key := range keys {
		if field, ok := fields[key]; !ok || field == nil {
			return nil, false
		}
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(value) != nil {
		return nil, false
	}
	limit := func(value, max int) bool { return value >= 1 && value <= max }
	id := func(value string) bool { return frozenID(value) && !strings.ContainsAny(value, "?#/\\") }
	token := func(value string) bool {
		return len(value) >= 8 && len(value) <= 4096 && !strings.ContainsAny(value, "\x00\r\n")
	}
	switch input := value.(type) {
	case *SearchFeedsArgs:
		filters, ok := fields["filters"].(map[string]any)
		if !ok {
			return nil, false
		}
		for _, value := range filters {
			if _, ok := value.(string); !ok {
				return nil, false
			}
		}
		if strings.TrimSpace(input.Keyword) == "" || utf8.RuneCountInString(input.Keyword) > 16000 || !limit(input.Limit, 200) {
			return nil, false
		}
		allowed := func(value string, choices string) bool {
			if value == "" {
				return true
			}
			for _, choice := range strings.Split(choices, "|") {
				if value == choice {
					return true
				}
			}
			return false
		}
		f := input.Filters
		if !allowed(f.SortBy, "综合|最新|最多点赞|最多评论|最多收藏") || !allowed(f.NoteType, "不限|视频|图文") || !allowed(f.PublishTime, "不限|一天内|一周内|半年内") || !allowed(f.SearchScope, "不限|已看过|未看过|已关注") || !allowed(f.Location, "不限|同城|附近") {
			return nil, false
		}
	case *FeedDetailArgs:
		if !id(input.FeedID) || !token(input.XsecToken) || !limit(input.Limit, 200) || !limit(input.ReplyLimit, 200) || (input.ScrollSpeed != "slow" && input.ScrollSpeed != "normal" && input.ScrollSpeed != "fast") {
			return nil, false
		}
	case *UserProfileArgs:
		if !id(input.UserID) || !token(input.XsecToken) {
			return nil, false
		}
	case *ListCollectionsArgs:
		if !limit(input.Limit, 500) {
			return nil, false
		}
	case *GetCollectionContentArgs:
		if !id(input.CollectionID) || !limit(input.Limit, 200) {
			return nil, false
		}
	case *ListSavedContentArgs:
		if !limit(input.Limit, 200) {
			return nil, false
		}
	}
	return value, true
}

func (s *guardedService) readPrivate(ctx context.Context, operation string, input any) (guardedFeedsResult, error) {
	return s.readInSession(ctx, func(ctx context.Context, session accountLeaseSession) (any, error) {
		reader, ok := session.(privateOperationReader)
		if !ok {
			return nil, errPrivateProvider
		}
		return reader.readPrivate(ctx, operation, input)
	})
}

func (s *controlledSession) readPrivate(ctx context.Context, operation string, raw any) (any, error) {
	page := s.page.Context(ctx)
	switch operation {
	case "search_feeds":
		input := raw.(*SearchFeedsArgs)
		f := input.Filters
		feeds, err := xiaohongshu.NewSearchAction(page).Search(ctx, input.Keyword, input.Limit, xiaohongshu.FilterOption{SortBy: f.SortBy, NoteType: f.NoteType, PublishTime: f.PublishTime, SearchScope: f.SearchScope, Location: f.Location})
		return &FeedsListResponse{Feeds: feeds, Count: len(feeds)}, err
	case "get_feed_detail":
		input := raw.(*FeedDetailArgs)
		config := xiaohongshu.CommentLoadConfig{ClickMoreReplies: input.ClickMoreReplies, MaxRepliesThreshold: input.ReplyLimit, MaxCommentItems: input.Limit, ScrollSpeed: input.ScrollSpeed}
		data, err := xiaohongshu.NewFeedDetailAction(page).GetFeedDetailWithConfig(ctx, input.FeedID, input.XsecToken, input.LoadAllComments, config)
		return &FeedDetailResponse{FeedID: input.FeedID, Data: data}, err
	case "user_profile":
		input := raw.(*UserProfileArgs)
		data, err := xiaohongshu.NewUserProfileAction(page).UserProfile(ctx, input.UserID, input.XsecToken)
		if err != nil || data == nil {
			return nil, errPrivateProvider
		}
		return &UserProfileResponse{UserBasicInfo: data.UserBasicInfo, Interactions: data.Interactions, Feeds: data.Feeds}, nil
	case "list_collections":
		items, err := xiaohongshu.NewSavedContentAction(page).ListCollections(ctx, raw.(*ListCollectionsArgs).Limit)
		if items == nil {
			items = []xiaohongshu.Collection{}
		}
		return items, err
	case "get_collection_content":
		input := raw.(*GetCollectionContentArgs)
		notes, total, err := xiaohongshu.NewSavedContentAction(page).GetCollectionContent(ctx, input.CollectionID, input.Limit)
		return &BoardNotesResponse{Notes: notes, Count: len(notes), Total: total}, err
	case "list_saved_content":
		feeds, err := xiaohongshu.NewSavedContentAction(page).ListSavedContent(ctx, raw.(*ListSavedContentArgs).Limit)
		return &FeedsListResponse{Feeds: feeds, Count: len(feeds)}, err
	default:
		return nil, errPrivateProvider
	}
}
