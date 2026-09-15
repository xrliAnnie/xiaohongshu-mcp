package main

import (
	"context"
	"errors"
	"github.com/go-rod/rod"
	"github.com/xpzouying/xiaohongshu-mcp/xiaohongshu"
	"net/url"
	"time"
)

type guardedWriteSession interface {
	accountLeaseSession
	writePage() *rod.Page
}
type guardedTokenResolver func(context.Context, internalPermit, frozenAccount, frozenTarget) (string, error)
type guardedCommand struct {
	operation                                 string
	image                                     *xiaohongshu.PublishImageContent
	video                                     *xiaohongshu.PublishVideoContent
	feedID, commentID, userID, content, token string
	undo                                      bool
}
type guardedDispatcher struct {
	resolve guardedTokenResolver
	run     func(context.Context, *rod.Page, guardedCommand) error
}

var errWriteOutcomeUnknown = errors.New("write_outcome_unknown")

func newGuardedDispatcher(resolve guardedTokenResolver) *guardedDispatcher {
	return &guardedDispatcher{resolve: resolve, run: runGuardedCommand}
}

// Only a journal-backed capability supplies content and the original session.
// No page, payload, resource token or media path is accepted from a commit caller.
func (d *guardedDispatcher) dispatch(ctx context.Context, v *verifiedWrite) error {
	if v == nil || v.lease == nil || v.payload == nil || v.used.Load() || ctx.Err() != nil || d.run == nil {
		return errWritePermit
	}
	session, ok := v.lease.session.(guardedWriteSession)
	if !ok || session.writePage() == nil {
		return errAccountMismatch
	}
	w, paths, err := v.payload.snapshot(time.Now().UnixMilli())
	if err != nil {
		return err
	}
	if v.payload.digest != v.permit.ContentDigest || v.lease.digest != v.payload.digest || w.Account.AccountUserID != v.permit.AccountUserID || w.Account.AccountEpoch != v.permit.AccountEpoch || w.Account.ProviderGeneration != v.permit.ProviderGeneration || v.lease.id != v.permit.LeaseID {
		return errAccountMismatch
	}
	token := ""
	if w.Target != nil {
		if d.resolve == nil || !journalID.MatchString(w.Target.FeedID) || w.Target.CommentID != nil && !journalID.MatchString(*w.Target.CommentID) || w.Target.UserID != nil && !journalID.MatchString(*w.Target.UserID) {
			return errFrozenWrite
		}
		token, err = d.resolve(ctx, v.permit, w.Account, *w.Target)
		if err != nil || token == "" || len(token) > 4096 {
			return errFrozenWrite
		}
	}
	command, err := commandForFrozen(w, paths, token)
	if err != nil {
		return err
	}
	if ctx.Err() != nil || time.Now().UnixMilli() >= v.permit.ExpiresAt || !v.take() {
		return errWritePermit
	}
	return d.run(ctx, session.writePage(), command)
}
func commandForFrozen(w frozenWrite, paths []string, token string) (guardedCommand, error) {
	c := guardedCommand{operation: w.OperationID, token: token}
	if !validFrozenWrite(w, time.Now().UnixMilli()) || len(paths) != len(w.Media) {
		return c, errFrozenWrite
	}
	var schedule *time.Time
	if w.Payload.ScheduleAt != nil {
		value, err := time.Parse(time.RFC3339Nano, *w.Payload.ScheduleAt)
		if err != nil {
			return c, errFrozenWrite
		}
		schedule = &value
	}
	switch w.OperationID {
	case "xiaohongshu.publish_content":
		c.image = &xiaohongshu.PublishImageContent{Title: *w.Payload.Title, Content: *w.Payload.Content, Tags: append([]string(nil), w.Payload.Tags...), ImagePaths: append([]string(nil), paths...), ScheduleTime: schedule, IsOriginal: *w.Payload.IsOriginal, Visibility: *w.Payload.Visibility}
	case "xiaohongshu.publish_with_video":
		c.video = &xiaohongshu.PublishVideoContent{Title: *w.Payload.Title, Content: *w.Payload.Content, Tags: append([]string(nil), w.Payload.Tags...), VideoPath: paths[0], ScheduleTime: schedule, IsOriginal: *w.Payload.IsOriginal, Visibility: *w.Payload.Visibility}
	default:
		if token == "" {
			return c, errFrozenWrite
		}
		c.feedID = w.Target.FeedID
		if w.Target.CommentID != nil {
			c.commentID = *w.Target.CommentID
		}
		if w.Target.UserID != nil {
			c.userID = *w.Target.UserID
		}
		if w.Payload.Content != nil {
			c.content = *w.Payload.Content
		}
		if w.Payload.Unlike != nil {
			c.undo = *w.Payload.Unlike
		}
		if w.Payload.Unfavorite != nil {
			c.undo = *w.Payload.Unfavorite
		}
	}
	return c, nil
}
func runGuardedCommand(ctx context.Context, page *rod.Page, c guardedCommand) error {
	page = page.Context(ctx)
	// Existing URL builders append this trusted value as a query component.
	token := url.QueryEscape(c.token)
	var err error
	switch c.operation {
	case "xiaohongshu.publish_content":
		action, e := xiaohongshu.NewPublishImageAction(page)
		if e != nil {
			return errWriteOutcomeUnknown
		}
		err = action.Publish(ctx, *c.image)
	case "xiaohongshu.publish_with_video":
		action, e := xiaohongshu.NewPublishVideoAction(page)
		if e != nil {
			return errWriteOutcomeUnknown
		}
		err = action.PublishVideo(ctx, *c.video)
	case "xiaohongshu.post_comment_to_feed":
		err = xiaohongshu.NewCommentFeedAction(page).PostComment(ctx, c.feedID, token, c.content)
	case "xiaohongshu.reply_comment_in_feed":
		// Never let the legacy lookup fall back to another comment by the same user.
		err = xiaohongshu.NewCommentFeedAction(page).ReplyToExactComment(ctx, c.feedID, token, c.commentID, c.userID, c.content)
	case "xiaohongshu.like_feed":
		action := xiaohongshu.NewLikeAction(page)
		if c.undo {
			return action.Unlike(ctx, c.feedID, token)
		}
		return action.Like(ctx, c.feedID, token)
	case "xiaohongshu.favorite_feed":
		action := xiaohongshu.NewFavoriteAction(page)
		if c.undo {
			return action.Unfavorite(ctx, c.feedID, token)
		}
		return action.Favorite(ctx, c.feedID, token)
	default:
		return errFrozenWrite
	}
	// Legacy publication/comment methods do not return a platform receipt.
	// A click alone cannot establish successful delivery; never fabricate success.
	_ = err
	return errWriteOutcomeUnknown
}
