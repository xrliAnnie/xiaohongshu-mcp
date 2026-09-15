package xiaohongshu

import (
	"context"
	"encoding/json"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/proto"
	"github.com/sirupsen/logrus"
)

// ActionResult 通用动作响应（点赞/收藏等）
type ActionResult struct {
	FeedID  string `json:"feed_id"`
	Success bool   `json:"success"`
	Message string `json:"message"`
}

// 选择器常量
const (
	SelectorLikeButton    = ".interact-container .left .like-lottie"
	SelectorCollectButton = ".interact-container .left .reds-icon.collect-icon"
)

// interactActionType 交互动作类型
type interactActionType string

const (
	actionLike       interactActionType = "点赞"
	actionFavorite   interactActionType = "收藏"
	actionUnlike     interactActionType = "取消点赞"
	actionUnfavorite interactActionType = "取消收藏"
)

type interactAction struct {
	page *rod.Page
}

func newInteractAction(page *rod.Page) *interactAction {
	return &interactAction{page: page}
}

func (a *interactAction) preparePage(ctx context.Context, actionType interactActionType, feedID, xsecToken string) *rod.Page {
	page := a.page.Context(ctx).Timeout(60 * time.Second)
	url := makeFeedDetailURL(feedID, xsecToken)
	logrus.Infof("Opening feed detail page for %s", actionType)

	page.MustNavigate(url)
	page.MustWaitDOMStable()
	time.Sleep(1 * time.Second)

	return page
}

func (a *interactAction) performClick(page *rod.Page, selector string) error {
	element, err := page.Element(selector)
	if err != nil {
		return err
	}
	return element.Click(proto.InputMouseButtonLeft, 1)
}

// LikeAction 负责处理点赞相关交互
type LikeAction struct {
	*interactAction
}

func NewLikeAction(page *rod.Page) *LikeAction {
	return &LikeAction{interactAction: newInteractAction(page)}
}

// Like 点赞指定笔记，如果已点赞则直接返回
func (a *LikeAction) Like(ctx context.Context, feedID, xsecToken string) error {
	return a.perform(ctx, feedID, xsecToken, true)
}

// Unlike 取消点赞指定笔记，如果未点赞则直接返回
func (a *LikeAction) Unlike(ctx context.Context, feedID, xsecToken string) error {
	return a.perform(ctx, feedID, xsecToken, false)
}

func (a *LikeAction) perform(ctx context.Context, feedID, xsecToken string, target bool) error {
	action := actionLike
	if !target {
		action = actionUnlike
	}
	page := a.preparePage(ctx, action, feedID, xsecToken)
	return singleToggle(ctx, target, func() (bool, error) { liked, _, err := a.getInteractState(page, feedID); return liked, err }, func() error { return a.performClick(page, SelectorLikeButton) }, waitToggleState)
}

// FavoriteAction 负责处理收藏相关交互
type FavoriteAction struct {
	*interactAction
}

func NewFavoriteAction(page *rod.Page) *FavoriteAction {
	return &FavoriteAction{interactAction: newInteractAction(page)}
}

// Favorite 收藏指定笔记，如果已收藏则直接返回
func (a *FavoriteAction) Favorite(ctx context.Context, feedID, xsecToken string) error {
	return a.perform(ctx, feedID, xsecToken, true)
}

// Unfavorite 取消收藏指定笔记，如果未收藏则直接返回
func (a *FavoriteAction) Unfavorite(ctx context.Context, feedID, xsecToken string) error {
	return a.perform(ctx, feedID, xsecToken, false)
}

func (a *FavoriteAction) perform(ctx context.Context, feedID, xsecToken string, target bool) error {
	action := actionFavorite
	if !target {
		action = actionUnfavorite
	}
	page := a.preparePage(ctx, action, feedID, xsecToken)
	return singleToggle(ctx, target, func() (bool, error) { _, collected, err := a.getInteractState(page, feedID); return collected, err }, func() error { return a.performClick(page, SelectorCollectButton) }, waitToggleState)
}

// getInteractState 从 __INITIAL_STATE__ 读取笔记的点赞/收藏状态
func (a *interactAction) getInteractState(page *rod.Page, feedID string) (liked bool, collected bool, err error) {

	result := page.MustEval(`() => {
		if (window.__INITIAL_STATE__ &&
		    window.__INITIAL_STATE__.note &&
		    window.__INITIAL_STATE__.note.noteDetailMap) {
			return JSON.stringify(window.__INITIAL_STATE__.note.noteDetailMap);
		}
		return "";
	}`).String()
	return parseInteractState(result, feedID)
}
func parseInteractState(raw, feedID string) (bool, bool, error) {
	var details map[string]struct {
		Note struct {
			InteractInfo struct {
				Liked     *bool `json:"liked"`
				Collected *bool `json:"collected"`
			} `json:"interactInfo"`
		} `json:"note"`
	}
	if json.Unmarshal([]byte(raw), &details) != nil {
		return false, false, errInteractionState
	}
	detail, ok := details[feedID]
	if !ok || detail.Note.InteractInfo.Liked == nil || detail.Note.InteractInfo.Collected == nil {
		return false, false, errInteractionState
	}
	return *detail.Note.InteractInfo.Liked, *detail.Note.InteractInfo.Collected, nil
}
