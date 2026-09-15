package main

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/go-rod/rod"
	"github.com/sirupsen/logrus"
	"github.com/xpzouying/headless_browser"
	"github.com/xpzouying/xiaohongshu-mcp/browser"
	"github.com/xpzouying/xiaohongshu-mcp/configs"
	"github.com/xpzouying/xiaohongshu-mcp/cookies"
	"github.com/xpzouying/xiaohongshu-mcp/pkg/downloader"
	"github.com/xpzouying/xiaohongshu-mcp/xiaohongshu"
)

var errLegacyWriteGate = errors.New("founder_write_gate_absent")

// XiaohongshuService 小红书业务服务
type XiaohongshuService struct{}

const (
	shortBrowserOperationTimeout = 90 * time.Second
	loginOperationTimeout        = 60 * time.Second
	savedContentTimeout          = 5 * time.Minute
	publishContentTimeout        = 8 * time.Minute
	publishVideoTimeout          = 15 * time.Minute
	feedDetailTimeout            = 12 * time.Minute
)

// NewXiaohongshuService 创建小红书服务实例
func NewXiaohongshuService() *XiaohongshuService {
	return &XiaohongshuService{}
}

// PublishRequest 发布请求
type PublishRequest struct {
	Title      string   `json:"title" binding:"required"`
	Content    string   `json:"content" binding:"required"`
	Images     []string `json:"images" binding:"required,min=1"`
	Tags       []string `json:"tags,omitempty"`
	ScheduleAt string   `json:"schedule_at,omitempty"` // 定时发布时间，ISO8601格式，为空则立即发布
	IsOriginal bool     `json:"is_original,omitempty"` // 是否声明原创
	Visibility string   `json:"visibility,omitempty"`  // 可见范围: "公开可见"(默认), "仅自己可见", "仅互关好友可见"
	Products   []string `json:"products,omitempty"`    // 商品关键词列表，用于绑定带货商品
}

// LoginStatusResponse 登录状态响应
type LoginStatusResponse struct {
	IsLoggedIn bool   `json:"is_logged_in"`
	Username   string `json:"username,omitempty"`
}

// LoginQrcodeResponse 登录扫码二维码
type LoginQrcodeResponse struct {
	Timeout    string `json:"timeout"`
	IsLoggedIn bool   `json:"is_logged_in"`
	Img        string `json:"img,omitempty"`
}

// PublishResponse 发布响应
type PublishResponse struct {
	Title   string `json:"title"`
	Content string `json:"content"`
	Images  int    `json:"images"`
	Status  string `json:"status"`
	PostID  string `json:"post_id,omitempty"`
}

// PublishVideoRequest 发布视频请求（仅支持本地单个视频文件）
type PublishVideoRequest struct {
	Title      string   `json:"title" binding:"required"`
	Content    string   `json:"content" binding:"required"`
	Video      string   `json:"video" binding:"required"`
	Tags       []string `json:"tags,omitempty"`
	ScheduleAt string   `json:"schedule_at,omitempty"` // 定时发布时间，ISO8601格式，为空则立即发布
	Visibility string   `json:"visibility,omitempty"`  // 可见范围: "公开可见"(默认), "仅自己可见", "仅互关好友可见"
	Products   []string `json:"products,omitempty"`    // 商品关键词列表，用于绑定带货商品
}

// PublishVideoResponse 发布视频响应
type PublishVideoResponse struct {
	Title   string `json:"title"`
	Content string `json:"content"`
	Video   string `json:"video"`
	Status  string `json:"status"`
	PostID  string `json:"post_id,omitempty"`
}

// FeedsListResponse Feeds列表响应
type FeedsListResponse struct {
	Feeds []xiaohongshu.Feed `json:"feeds"`
	Count int                `json:"count"`
}

// BoardNotesResponse 专辑笔记响应
//
// Count = 实际枚举到的可服务笔记数；Total = 收藏夹 saved 计数（含已删除/不可见，
// 来自 board.boardDetails，0 表示未读到）。Count < Total 表示有笔记已被作者删除/
// 设为私密，平台 feed 不再返回 —— 任何方式都拿不到，属正常。
type BoardNotesResponse struct {
	Notes []xiaohongshu.BoardNote `json:"notes"`
	Count int                     `json:"count"`
	Total int                     `json:"total"`
}

// UserProfileResponse 用户主页响应
type UserProfileResponse struct {
	UserBasicInfo xiaohongshu.UserBasicInfo      `json:"userBasicInfo"`
	Interactions  []xiaohongshu.UserInteractions `json:"interactions"`
	Feeds         []xiaohongshu.Feed             `json:"feeds"`
}

// DeleteCookies 删除 cookies 文件，用于登录重置
func (s *XiaohongshuService) DeleteCookies(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, loginOperationTimeout)
	defer cancel()

	if err := ctx.Err(); err != nil {
		return err
	}
	cookiePath := cookies.GetCookiesFilePath()
	cookieLoader := cookies.NewLoadCookie(cookiePath)
	return cookieLoader.DeleteCookies()
}

// CheckLoginStatus 检查登录状态
func (s *XiaohongshuService) CheckLoginStatus(ctx context.Context) (*LoginStatusResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, loginOperationTimeout)
	defer cancel()

	b := newBrowser()
	defer b.Close()

	page := b.NewPage()
	defer page.Close()

	loginAction := xiaohongshu.NewLogin(page)

	isLoggedIn, err := loginAction.CheckLoginStatus(ctx)
	if err != nil {
		return nil, err
	}

	response := &LoginStatusResponse{
		IsLoggedIn: isLoggedIn,
		Username:   configs.Username,
	}

	return response, nil
}

// GetLoginQrcode 获取登录的扫码二维码
func (s *XiaohongshuService) GetLoginQrcode(ctx context.Context) (*LoginQrcodeResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, loginOperationTimeout)
	defer cancel()

	b := newBrowser()
	var page *rod.Page
	var cleanupOnce sync.Once
	cleanup := func() {
		cleanupOnce.Do(func() {
			if page != nil {
				_ = page.Close()
			}
			b.Close()
		})
	}
	handedOff := false
	defer func() {
		if !handedOff {
			cleanup()
		}
	}()

	page = b.NewPage()

	loginAction := xiaohongshu.NewLogin(page)

	img, loggedIn, err := loginAction.FetchQrcodeImage(ctx)
	if err != nil {
		return nil, err
	}

	timeout := 4 * time.Minute

	if !loggedIn {
		go func() {
			runWithPanicSafeCleanup(cleanup, func() {
				ctxTimeout, cancel := context.WithTimeout(context.Background(), timeout)
				defer cancel()

				if loginAction.WaitForLogin(ctxTimeout) {
					if er := saveCookies(page); er != nil {
						logrus.Errorf("failed to save cookies: %v", er)
					}
				}
			})
		}()
		handedOff = true
	}

	return &LoginQrcodeResponse{
		Timeout: func() string {
			if loggedIn {
				return "0s"
			}
			return timeout.String()
		}(),
		Img:        img,
		IsLoggedIn: loggedIn,
	}, nil
}

func runWithPanicSafeCleanup(cleanup, work func()) {
	defer func() {
		if recovered := recover(); recovered != nil {
			logrus.Errorf("login background task panicked: %v", recovered)
		}
	}()
	defer func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				logrus.Errorf("login background cleanup panicked: %v", recovered)
			}
		}()
		cleanup()
	}()
	work()
}

// PublishContent 发布内容
func (s *XiaohongshuService) PublishContent(ctx context.Context, req *PublishRequest) (*PublishResponse, error) {
	// Legacy entrypoints have no verified context; only the private provider may write.
	return nil, errLegacyWriteGate
}

// processImages 处理图片列表，支持URL下载和本地路径
func (s *XiaohongshuService) processImages(images []string) ([]string, error) {
	processor := downloader.NewImageProcessor()
	return processor.ProcessImages(images)
}

// publishContent 执行内容发布
func (s *XiaohongshuService) publishContent(ctx context.Context, content xiaohongshu.PublishImageContent) error {
	// Legacy entrypoints have no verified context; only the private provider may write.
	return errLegacyWriteGate
}

// PublishVideo 发布视频（本地文件）
func (s *XiaohongshuService) PublishVideo(ctx context.Context, req *PublishVideoRequest) (*PublishVideoResponse, error) {
	// Legacy entrypoints have no verified context; only the private provider may write.
	return nil, errLegacyWriteGate
}

// publishVideo 执行视频发布
func (s *XiaohongshuService) publishVideo(ctx context.Context, content xiaohongshu.PublishVideoContent) error {
	// Legacy entrypoints have no verified context; only the private provider may write.
	return errLegacyWriteGate
}

// ListFeeds 获取Feeds列表
func (s *XiaohongshuService) ListFeeds(ctx context.Context) (*FeedsListResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, shortBrowserOperationTimeout)
	defer cancel()

	b := newBrowser()
	defer b.Close()

	page := b.NewPage()
	defer page.Close()

	// 创建 Feeds 列表 action
	action := xiaohongshu.NewFeedsListAction(page)

	// 获取 Feeds 列表
	feeds, err := action.GetFeedsList(ctx)
	if err != nil {
		logrus.Errorf("获取 Feeds 列表失败: %v", err)
		return nil, err
	}

	response := &FeedsListResponse{
		Feeds: feeds,
		Count: len(feeds),
	}

	return response, nil
}

func (s *XiaohongshuService) SearchFeeds(ctx context.Context, keyword string, limit int, filters ...xiaohongshu.FilterOption) (*FeedsListResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, shortBrowserOperationTimeout)
	defer cancel()

	b := newBrowser()
	defer b.Close()

	page := b.NewPage()
	defer page.Close()

	action := xiaohongshu.NewSearchAction(page)

	feeds, err := action.Search(ctx, keyword, limit, filters...)
	if err != nil {
		return nil, err
	}

	response := &FeedsListResponse{
		Feeds: feeds,
		Count: len(feeds),
	}

	return response, nil
}

// GetFeedDetail 获取Feed详情
func (s *XiaohongshuService) GetFeedDetail(ctx context.Context, feedID, xsecToken string, loadAllComments bool) (*FeedDetailResponse, error) {
	return s.GetFeedDetailWithConfig(ctx, feedID, xsecToken, loadAllComments, xiaohongshu.DefaultCommentLoadConfig())
}

// GetFeedDetailWithConfig 使用配置获取Feed详情
func (s *XiaohongshuService) GetFeedDetailWithConfig(ctx context.Context, feedID, xsecToken string, loadAllComments bool, config xiaohongshu.CommentLoadConfig) (*FeedDetailResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, feedDetailTimeout)
	defer cancel()

	b := newBrowser()
	defer b.Close()

	page := b.NewPage()
	defer page.Close()

	// 创建 Feed 详情 action
	action := xiaohongshu.NewFeedDetailAction(page)

	// 获取 Feed 详情
	result, err := action.GetFeedDetailWithConfig(ctx, feedID, xsecToken, loadAllComments, config)
	if err != nil {
		return nil, err
	}

	response := &FeedDetailResponse{
		FeedID: feedID,
		Data:   result,
	}

	return response, nil
}

// UserProfile 获取用户信息
func (s *XiaohongshuService) UserProfile(ctx context.Context, userID, xsecToken string) (*UserProfileResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, shortBrowserOperationTimeout)
	defer cancel()

	b := newBrowser()
	defer b.Close()

	page := b.NewPage()
	defer page.Close()

	action := xiaohongshu.NewUserProfileAction(page)

	result, err := action.UserProfile(ctx, userID, xsecToken)
	if err != nil {
		return nil, err
	}
	response := &UserProfileResponse{
		UserBasicInfo: result.UserBasicInfo,
		Interactions:  result.Interactions,
		Feeds:         result.Feeds,
	}

	return response, nil

}

// PostCommentToFeed 发表评论到Feed
func (s *XiaohongshuService) PostCommentToFeed(ctx context.Context, feedID, xsecToken, content string) (*PostCommentResponse, error) {
	// Legacy entrypoints have no verified context; only the private provider may write.
	return nil, errLegacyWriteGate
}

// LikeFeed 点赞笔记
func (s *XiaohongshuService) LikeFeed(ctx context.Context, feedID, xsecToken string) (*ActionResult, error) {
	// Legacy entrypoints have no verified context; only the private provider may write.
	return nil, errLegacyWriteGate
}

// UnlikeFeed 取消点赞笔记
func (s *XiaohongshuService) UnlikeFeed(ctx context.Context, feedID, xsecToken string) (*ActionResult, error) {
	// Legacy entrypoints have no verified context; only the private provider may write.
	return nil, errLegacyWriteGate
}

// FavoriteFeed 收藏笔记
func (s *XiaohongshuService) FavoriteFeed(ctx context.Context, feedID, xsecToken string) (*ActionResult, error) {
	// Legacy entrypoints have no verified context; only the private provider may write.
	return nil, errLegacyWriteGate
}

// UnfavoriteFeed 取消收藏笔记
func (s *XiaohongshuService) UnfavoriteFeed(ctx context.Context, feedID, xsecToken string) (*ActionResult, error) {
	// Legacy entrypoints have no verified context; only the private provider may write.
	return nil, errLegacyWriteGate
}

// ReplyCommentToFeed 回复指定评论
func (s *XiaohongshuService) ReplyCommentToFeed(ctx context.Context, feedID, xsecToken, commentID, userID, content string) (*ReplyCommentResponse, error) {
	// Legacy entrypoints have no verified context; only the private provider may write.
	return nil, errLegacyWriteGate
}

func newBrowser() *headless_browser.Browser {
	return browser.NewBrowser(configs.IsHeadless(), browser.WithBinPath(configs.GetBinPath()))
}

func saveCookies(page *rod.Page) error {
	cks, err := page.Browser().GetCookies()
	if err != nil {
		return err
	}

	data, err := json.Marshal(cks)
	if err != nil {
		return err
	}

	cookieLoader := cookies.NewLoadCookie(cookies.GetCookiesFilePath())
	return cookieLoader.SaveCookies(data)
}

// withBrowserPage 执行需要浏览器页面的操作的通用函数
func withBrowserPage(fn func(*rod.Page) error) error {
	b := newBrowser()
	defer b.Close()

	page := b.NewPage()
	defer page.Close()

	return fn(page)
}

// GetMyProfile 获取当前登录用户的个人信息
func (s *XiaohongshuService) GetMyProfile(ctx context.Context) (*UserProfileResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, shortBrowserOperationTimeout)
	defer cancel()

	var result *xiaohongshu.UserProfileResponse
	var err error

	err = withBrowserPage(func(page *rod.Page) error {
		action := xiaohongshu.NewUserProfileAction(page)
		result, err = action.GetMyProfileViaSidebar(ctx)
		return err
	})

	if err != nil {
		return nil, err
	}

	response := &UserProfileResponse{
		UserBasicInfo: result.UserBasicInfo,
		Interactions:  result.Interactions,
		Feeds:         result.Feeds,
	}

	return response, nil
}

// ListCollections 列出当前登录用户的收藏夹
func (s *XiaohongshuService) ListCollections(ctx context.Context, limit int) ([]xiaohongshu.Collection, error) {
	ctx, cancel := context.WithTimeout(ctx, savedContentTimeout)
	defer cancel()

	b := newBrowser()
	defer b.Close()

	page := b.NewPage()
	defer page.Close()

	action := xiaohongshu.NewSavedContentAction(page)
	return action.ListCollections(ctx, limit)
}

// GetCollectionContent 获取指定专辑的内容
func (s *XiaohongshuService) GetCollectionContent(ctx context.Context, collectionID string, limit int) (*BoardNotesResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, savedContentTimeout)
	defer cancel()

	b := newBrowser()
	defer b.Close()

	page := b.NewPage()
	defer page.Close()

	action := xiaohongshu.NewSavedContentAction(page)
	notes, total, err := action.GetCollectionContent(ctx, collectionID, limit)
	if err != nil {
		return nil, err
	}

	return &BoardNotesResponse{
		Notes: notes,
		Count: len(notes),
		Total: total,
	}, nil
}

// ListSavedContent 获取全部收藏内容
func (s *XiaohongshuService) ListSavedContent(ctx context.Context, limit int) (*FeedsListResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, savedContentTimeout)
	defer cancel()

	b := newBrowser()
	defer b.Close()

	page := b.NewPage()
	defer page.Close()

	action := xiaohongshu.NewSavedContentAction(page)
	feeds, err := action.ListSavedContent(ctx, limit)
	if err != nil {
		return nil, err
	}

	return &FeedsListResponse{
		Feeds: feeds,
		Count: len(feeds),
	}, nil
}
