package xiaohongshu

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/proto"
	"github.com/sirupsen/logrus"
)

// ErrNotLoggedIn 未登录错误（sentinel，用 errors.Is 判定）
var ErrNotLoggedIn = errors.New("请先登录小红书，使用 check_login_status 检查登录状态")

// Collection 收藏夹信息
type Collection struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Count int    `json:"count"`
	Cover string `json:"cover,omitempty"`
}

// SavedContentAction 收藏内容浏览
type SavedContentAction struct {
	page *rod.Page
}

func NewSavedContentAction(page *rod.Page) *SavedContentAction {
	pp := page.Timeout(90 * time.Second)
	return &SavedContentAction{page: pp}
}

// ListCollections 列出当前登录用户的所有收藏夹
func (s *SavedContentAction) ListCollections(ctx context.Context) ([]Collection, error) {
	page := s.page.Context(ctx)

	// 安全导航到个人主页
	profileURL, err := s.safeNavigateToProfile(ctx)
	if err != nil {
		return nil, err
	}

	// 导航到收藏 tab
	if err := s.navigateToCollectTab(page, profileURL); err != nil {
		return nil, fmt.Errorf("导航到收藏页失败: %w", err)
	}

	// 等待 state 加载
	page.MustWait(`() => window.__INITIAL_STATE__ !== undefined`)

	// 两层探测：先打印 root keys
	rootKeys := s.debugRootKeys(page)
	logrus.Infof("收藏页 __INITIAL_STATE__ root keys: %v", rootKeys)

	// 读取收藏夹列表
	raw := page.MustEval(readCollectFoldersJS).String()
	if raw == "" {
		// 尝试探测更多 key 信息
		s.debugSubKeys(page)
		logrus.Warn("未找到收藏夹数据，返回空列表")
		return []Collection{}, nil
	}

	var collections []Collection
	if err := json.Unmarshal([]byte(raw), &collections); err != nil {
		// 打印原始数据帮助调试格式不匹配
		if len(raw) > 100 {
			logrus.Warnf("收藏夹数据解析失败: %v, 原始数据(前100字符): %s", err, raw[:100])
		} else {
			logrus.Warnf("收藏夹数据解析失败: %v, 原始数据: %s", err, raw)
		}
		return nil, fmt.Errorf("收藏夹数据格式不匹配，请检查服务端日志")
	}

	logrus.Infof("获取到 %d 个收藏夹", len(collections))
	return collections, nil
}

// GetCollectionContent 获取指定收藏夹中的笔记
func (s *SavedContentAction) GetCollectionContent(ctx context.Context, collectionID string, limit int) ([]Feed, error) {
	page := s.page.Context(ctx)

	// 安全导航到个人主页
	profileURL, err := s.safeNavigateToProfile(ctx)
	if err != nil {
		return nil, err
	}

	// 构造收藏夹详情页 URL
	collectionURL := buildCollectionURL(profileURL, collectionID)
	logrus.Infof("导航到收藏夹: %s", collectionURL)

	if err := rod.Try(func() {
		page.MustNavigate(collectionURL)
		page.MustWaitStable()
	}); err != nil {
		return nil, fmt.Errorf("导航到收藏夹页面失败: %w", err)
	}

	// 检查页面是否可访问
	if err := checkPageAccessible(page); err != nil {
		return nil, fmt.Errorf("收藏夹不存在或无法访问: %w", err)
	}

	// 等待 state 加载
	page.MustWait(`() => window.__INITIAL_STATE__ !== undefined`)

	// 读取收藏内容
	feeds, err := s.readCollectFeeds(page)
	if err != nil {
		return nil, err
	}

	// 翻页加载更多
	if limit > 0 && len(feeds) < limit {
		feeds = s.scrollAndCollect(page, feeds, limit)
	} else if limit > 0 && len(feeds) > limit {
		feeds = feeds[:limit]
	}

	logrus.Infof("收藏夹 %s 获取到 %d 条笔记", collectionID, len(feeds))
	return feeds, nil
}

// ListSavedContent 获取全部收藏内容
func (s *SavedContentAction) ListSavedContent(ctx context.Context, limit int) ([]Feed, error) {
	page := s.page.Context(ctx)

	// 安全导航到个人主页
	profileURL, err := s.safeNavigateToProfile(ctx)
	if err != nil {
		return nil, err
	}

	// 导航到收藏 tab
	if err := s.navigateToCollectTab(page, profileURL); err != nil {
		return nil, fmt.Errorf("导航到收藏页失败: %w", err)
	}

	// 等待 state 加载
	page.MustWait(`() => window.__INITIAL_STATE__ !== undefined`)

	// 读取收藏内容
	feeds, err := s.readCollectFeeds(page)
	if err != nil {
		return nil, err
	}

	// 翻页加载更多
	if limit > 0 && len(feeds) < limit {
		feeds = s.scrollAndCollect(page, feeds, limit)
	} else if limit > 0 && len(feeds) > limit {
		feeds = feeds[:limit]
	}

	logrus.Infof("全部收藏获取到 %d 条笔记", len(feeds))
	return feeds, nil
}

// ========== 导航相关 ==========

// safeNavigateToProfile 安全导航到个人主页（非 Must API，显式处理未登录）
func (s *SavedContentAction) safeNavigateToProfile(ctx context.Context) (string, error) {
	page := s.page.Context(ctx)

	// 导航到 explore 页面
	if err := rod.Try(func() {
		page.MustNavigate("https://www.xiaohongshu.com/explore")
		page.MustWaitStable()
	}); err != nil {
		return "", fmt.Errorf("导航到首页失败: %w", err)
	}

	// 多信号登录检测
	if err := checkLoginState(page); err != nil {
		return "", err
	}

	// 查找侧边栏 "我" 入口（带超时）
	el, err := page.Timeout(5 * time.Second).Element(
		`div.main-container li.user.side-bar-component a.link-wrapper span.channel`,
	)
	if err != nil {
		// 侧边栏用户入口不存在 = 未登录
		return "", ErrNotLoggedIn
	}

	// 点击并等待导航
	if err := el.Click(proto.InputMouseButtonLeft, 1); err != nil {
		return "", fmt.Errorf("点击用户入口失败: %w", err)
	}

	if err := rod.Try(func() {
		page.MustWaitStable()
	}); err != nil {
		return "", fmt.Errorf("等待个人主页加载失败: %w", err)
	}

	// 二次登录检测
	if err := checkLoginState(page); err != nil {
		return "", err
	}

	// 返回当前 URL
	info, err := page.Info()
	if err != nil {
		return "", fmt.Errorf("获取页面信息失败: %w", err)
	}

	logrus.Infof("已导航到个人主页: %s", info.URL)
	return info.URL, nil
}

// navigateToCollectTab 在个人主页基础上导航到收藏 tab
func (s *SavedContentAction) navigateToCollectTab(page *rod.Page, profileURL string) error {
	// 在 profile URL 基础上追加 tab=board（小红书收藏 tab 实际参数名为 board）
	collectURL := appendTabToURL(profileURL, "board")
	logrus.Infof("导航到收藏 tab: %s", collectURL)

	if err := rod.Try(func() {
		page.MustNavigate(collectURL)
		page.MustWaitStable()
	}); err != nil {
		// URL 导航失败，尝试点击收藏 tab
		logrus.Warnf("URL 导航到收藏 tab 失败，尝试 DOM 点击: %v", err)
		return s.clickCollectTab(page)
	}

	return nil
}

// clickCollectTab 通过 DOM 点击收藏 tab（fallback）
func (s *SavedContentAction) clickCollectTab(page *rod.Page) error {
	// 尝试点击收藏 tab 元素
	tabSelectors := []string{
		`div.user-tab span:has-text("收藏")`,
		`[class*="tab"] [class*="collect"]`,
		`[class*="tab"] [class*="board"]`,
		`a[href*="tab=board"]`,
		`a[href*="tab=collect"]`,
	}

	for _, sel := range tabSelectors {
		el, err := page.Timeout(3 * time.Second).Element(sel)
		if err != nil {
			continue
		}
		if err := el.Click(proto.InputMouseButtonLeft, 1); err != nil {
			continue
		}
		page.WaitStable(2 * time.Second)
		logrus.Infof("通过 DOM 点击收藏 tab 成功: %s", sel)
		return nil
	}

	return fmt.Errorf("无法找到收藏 tab 元素")
}

// ========== 数据读取 ==========

// readCollectFeeds 从 __INITIAL_STATE__ 读取收藏笔记
func (s *SavedContentAction) readCollectFeeds(page *rod.Page) ([]Feed, error) {
	raw := page.MustEval(readCollectFeedsJS).String()
	if raw == "" {
		// 探测并打印调试信息
		rootKeys := s.debugRootKeys(page)
		s.debugSubKeys(page)
		logrus.Warnf("未找到收藏内容数据，root keys: %v", rootKeys)
		return nil, fmt.Errorf("无法读取收藏数据，state key 未匹配，请检查服务端日志")
	}

	return flattenFeeds([]byte(raw))
}

// flattenFeeds 归一化 Feed 数据，统一处理 []Feed 和 [][]Feed
func flattenFeeds(raw []byte) ([]Feed, error) {
	// 先尝试 []Feed
	var feeds []Feed
	if err := json.Unmarshal(raw, &feeds); err == nil {
		return feeds, nil
	}

	// 再尝试 [][]Feed（双重数组，与 user.notes 格式一致）
	var nestedFeeds [][]Feed
	if err := json.Unmarshal(raw, &nestedFeeds); err == nil {
		var result []Feed
		for _, group := range nestedFeeds {
			if len(group) > 0 {
				result = append(result, group...)
			}
		}
		return result, nil
	}

	return nil, fmt.Errorf("无法解析收藏数据格式")
}

// ========== 翻页 ==========

// scrollAndCollect 滚动加载更多收藏内容
func (s *SavedContentAction) scrollAndCollect(page *rod.Page, initialFeeds []Feed, limit int) []Feed {
	seen := make(map[string]bool, len(initialFeeds))
	var allFeeds []Feed
	for _, f := range initialFeeds {
		if !seen[f.ID] {
			seen[f.ID] = true
			allFeeds = append(allFeeds, f)
		}
	}

	if len(allFeeds) >= limit {
		return allFeeds[:limit]
	}

	const maxStale = 5
	staleCount := 0

	for staleCount < maxStale && len(allFeeds) < limit {
		// 滚动触发懒加载
		if _, err := page.Eval(`() => window.scrollBy(0, 1500)`); err != nil {
			logrus.Warnf("收藏翻页滚动失败: %v", err)
			break
		}
		time.Sleep(2 * time.Second)

		_ = page.WaitStable(1 * time.Second)

		// 重读收藏数据
		obj, err := page.Eval(readCollectFeedsJS)
		if err != nil {
			logrus.Warnf("收藏翻页读取失败: %v", err)
			break
		}

		raw := obj.Value.String()
		if raw == "" {
			staleCount++
			continue
		}

		feeds, err := flattenFeeds([]byte(raw))
		if err != nil {
			logrus.Warnf("收藏翻页解析失败: %v", err)
			staleCount++
			continue
		}

		newCount := 0
		for _, f := range feeds {
			if !seen[f.ID] {
				seen[f.ID] = true
				allFeeds = append(allFeeds, f)
				newCount++
			}
		}

		if newCount == 0 {
			staleCount++
		} else {
			staleCount = 0
		}

		logrus.Infof("收藏翻页: 本次新增 %d 条, 总计 %d/%d", newCount, len(allFeeds), limit)
	}

	if len(allFeeds) > limit {
		return allFeeds[:limit]
	}
	return allFeeds
}

// ========== 登录检测 ==========

// checkLoginState 多信号登录检测
func checkLoginState(page *rod.Page) error {
	// 信号 1: URL 包含登录路径
	if info, err := page.Info(); err == nil {
		url := info.URL
		if strings.Contains(url, "/login") || strings.Contains(url, "signin") {
			return ErrNotLoggedIn
		}
	}

	// 信号 2: 页面内出现登录弹窗
	loginSelectors := []string{
		".login-container",
		".qr-login-container",
		"[class*='login-modal']",
		"[class*='login-dialog']",
	}
	for _, sel := range loginSelectors {
		el, err := page.Timeout(1 * time.Second).Element(sel)
		if err != nil {
			continue
		}
		if visible, _ := el.Visible(); visible {
			return ErrNotLoggedIn
		}
	}

	return nil
}

// ========== URL 工具 ==========

// appendTabToURL 在 URL 上追加 tab 参数
func appendTabToURL(profileURL, tab string) string {
	if strings.Contains(profileURL, "?") {
		return profileURL + "&tab=" + tab
	}
	return profileURL + "?tab=" + tab
}

// buildCollectionURL 构造收藏夹详情页 URL
func buildCollectionURL(profileURL, collectionID string) string {
	// 从 profile URL 中提取基础路径（去掉 query 参数）
	base := profileURL
	if idx := strings.Index(base, "?"); idx != -1 {
		base = base[:idx]
	}
	return fmt.Sprintf("%s/board/%s", base, collectionID)
}

// ========== 调试工具 ==========

// debugRootKeys 打印 __INITIAL_STATE__ 的 root keys
func (s *SavedContentAction) debugRootKeys(page *rod.Page) []string {
	raw := page.MustEval(debugRootKeysJS).String()
	if raw == "" {
		return nil
	}
	var keys []string
	if err := json.Unmarshal([]byte(raw), &keys); err != nil {
		return nil
	}
	return keys
}

// debugSubKeys 打印候选 root 下的 sub keys
func (s *SavedContentAction) debugSubKeys(page *rod.Page) {
	raw := page.MustEval(debugSubKeysJS).String()
	if raw != "" {
		logrus.Infof("候选 root 的 sub keys: %s", raw)
	}
}

// ========== JS 读取表达式 ==========

// debugRootKeysJS 探测 __INITIAL_STATE__ 的 root keys
const debugRootKeysJS = `() => {
	const state = window.__INITIAL_STATE__;
	return state ? JSON.stringify(Object.keys(state)) : "";
}`

// debugSubKeysJS 打印候选 root 下的 sub keys
const debugSubKeysJS = `() => {
	const state = window.__INITIAL_STATE__;
	if (!state) return "";
	const result = {};
	const rootNames = ["user", "collect", "collection", "board"];
	for (const name of rootNames) {
		if (state[name]) {
			result[name] = Object.keys(state[name]);
		}
	}
	return JSON.stringify(result);
}`

// readCollectFeedsJS 读取收藏笔记（多 root + 多 sub key 探测）
// 只返回 array 数据，忽略空 object {}
const readCollectFeedsJS = `() => {
	const state = window.__INITIAL_STATE__;
	if (!state) return "";
	const roots = [state.board, state.user, state.collect, state.collection];
	for (const root of roots) {
		if (!root) continue;
		const candidates = [root.boardFeedsMap, root.boardListData,
		                    root.collect, root.collectNotes, root.collections,
		                    root.notes, root.feeds];
		for (const c of candidates) {
			if (!c) continue;
			const data = c.value !== undefined ? c.value : c._value;
			if (!data) continue;
			if (Array.isArray(data) && data.length > 0) return JSON.stringify(data);
			if (typeof data === "object" && !Array.isArray(data) && Object.keys(data).length > 0) return JSON.stringify(data);
		}
	}
	return "";
}`

// readCollectFoldersJS 读取收藏夹列表（多 root + 多 sub key 探测）
// 只返回 array 数据，忽略空 object {}
const readCollectFoldersJS = `() => {
	const state = window.__INITIAL_STATE__;
	if (!state) return "";
	const roots = [state.board, state.user, state.collect, state.collection];
	for (const root of roots) {
		if (!root) continue;
		const candidates = [root.boardListData, root.userBoardList,
		                    root.collectFolders, root.boards,
		                    root.collectionFolders, root.folders];
		for (const c of candidates) {
			if (!c) continue;
			const data = c.value !== undefined ? c.value : c._value;
			if (!data) continue;
			if (Array.isArray(data) && data.length > 0) return JSON.stringify(data);
			if (typeof data === "object" && !Array.isArray(data) && Object.keys(data).length > 0) return JSON.stringify(data);
		}
	}
	return "";
}`
