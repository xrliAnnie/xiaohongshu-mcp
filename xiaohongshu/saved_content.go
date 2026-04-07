package xiaohongshu

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/proto"
	"github.com/sirupsen/logrus"
)

// ErrNotLoggedIn 未登录错误（sentinel，用 errors.Is 判定）
var ErrNotLoggedIn = errors.New("请先登录小红书，使用 check_login_status 检查登录状态")

// validBoardID 校验专辑 ID 格式（防止 JS 注入）
var validBoardID = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

// Collection 收藏夹信息
type Collection struct {
	ID      string   `json:"id"`
	Name    string   `json:"name"`
	Total   int      `json:"total"`
	Desc    string   `json:"desc"`
	Images  []string `json:"images,omitempty"`
	Privacy int      `json:"privacy"`
	Fans    int      `json:"fans"`
}

// BoardNote 专辑内的笔记（结构与 Feed 不同）
type BoardNote struct {
	NoteID         string `json:"noteId"`
	DisplayTitle   string `json:"displayTitle"`
	XsecToken      string `json:"xsecToken"`
	Type           string `json:"type"`
	LastUpdateTime int64  `json:"lastUpdateTime"`
	Cover          Cover  `json:"cover"`
}

// SavedContentAction 收藏内容浏览
type SavedContentAction struct {
	page *rod.Page
}

func NewSavedContentAction(page *rod.Page) *SavedContentAction {
	pp := page.Timeout(180 * time.Second)
	return &SavedContentAction{page: pp}
}

// ListCollections 列出当前登录用户的所有收藏夹
func (s *SavedContentAction) ListCollections(ctx context.Context, limit int) ([]Collection, error) {
	page := s.page.Context(ctx)

	profileURL, err := s.safeNavigateToProfile(ctx)
	if err != nil {
		return nil, err
	}

	// 先导航到 ?tab=fav（收藏 tab），URL 直接带 subTab=board 不会触发数据加载
	favURL := appendTabToURL(profileURL, "fav")
	logrus.Infof("导航到收藏 tab: %s", favURL)

	if err := rod.Try(func() {
		page.MustNavigate(favURL)
		page.MustWaitStable()
	}); err != nil {
		return nil, fmt.Errorf("导航到收藏 tab 失败: %w", err)
	}

	page.MustWait(`() => window.__INITIAL_STATE__ !== undefined`)

	// DOM 点击"专辑" subTab 触发数据加载
	if err := s.clickBoardSubTab(page); err != nil {
		return nil, fmt.Errorf("点击专辑 subTab 失败: %w", err)
	}

	// 等待 board.userBoardList 数据加载
	if err := s.waitForBoardList(page); err != nil {
		return nil, err
	}

	collections, err := s.readCollections(page)
	if err != nil {
		return nil, err
	}

	// 翻页加载更多
	if limit > 0 && len(collections) < limit {
		collections = s.scrollAndCollectCollections(page, collections, limit)
	} else if limit > 0 && len(collections) > limit {
		collections = collections[:limit]
	}

	logrus.Infof("获取到 %d 个收藏夹", len(collections))
	return collections, nil
}

// GetCollectionContent 获取指定专辑中的笔记
func (s *SavedContentAction) GetCollectionContent(ctx context.Context, collectionID string, limit int) ([]BoardNote, error) {
	if !validBoardID.MatchString(collectionID) {
		return nil, fmt.Errorf("无效的专辑 ID: %s", collectionID)
	}

	page := s.page.Context(ctx)

	// 专辑页使用独立 URL，无需经过个人主页
	boardURL := fmt.Sprintf("https://www.xiaohongshu.com/board/%s", collectionID)
	logrus.Infof("导航到专辑: %s", boardURL)

	if err := rod.Try(func() {
		page.MustNavigate(boardURL)
		page.MustWaitStable()
	}); err != nil {
		return nil, fmt.Errorf("导航到专辑页面失败: %w", err)
	}

	// 登录态检测（专辑页不经过 safeNavigateToProfile，需单独检查）
	if err := checkLoginState(page); err != nil {
		return nil, err
	}

	if err := checkPageAccessible(page); err != nil {
		return nil, fmt.Errorf("专辑不存在或无法访问: %w", err)
	}

	page.MustWait(`() => window.__INITIAL_STATE__ !== undefined`)

	notes, err := s.readBoardNotes(page, collectionID)
	if err != nil {
		return nil, err
	}

	if limit > 0 && len(notes) < limit {
		notes = s.scrollAndCollectBoardNotes(page, collectionID, notes, limit)
	} else if limit > 0 && len(notes) > limit {
		notes = notes[:limit]
	}

	logrus.Infof("专辑 %s 获取到 %d 条笔记", collectionID, len(notes))
	return notes, nil
}

// ListSavedContent 获取全部收藏内容
func (s *SavedContentAction) ListSavedContent(ctx context.Context, limit int) ([]Feed, error) {
	page := s.page.Context(ctx)

	profileURL, err := s.safeNavigateToProfile(ctx)
	if err != nil {
		return nil, err
	}

	// 导航到 ?tab=fav（收藏笔记列表）
	favURL := appendTabToURL(profileURL, "fav")
	logrus.Infof("导航到收藏内容: %s", favURL)

	if err := rod.Try(func() {
		page.MustNavigate(favURL)
		page.MustWaitStable()
	}); err != nil {
		return nil, fmt.Errorf("导航到收藏页失败: %w", err)
	}

	page.MustWait(`() => window.__INITIAL_STATE__ !== undefined`)

	feeds, err := s.readSavedFeeds(page)
	if err != nil {
		return nil, err
	}

	if limit > 0 && len(feeds) < limit {
		feeds = s.scrollAndCollectFeeds(page, feeds, limit)
	} else if limit > 0 && len(feeds) > limit {
		feeds = feeds[:limit]
	}

	logrus.Infof("全部收藏获取到 %d 条笔记", len(feeds))
	return feeds, nil
}

// ========== 导航 ==========

// safeNavigateToProfile 安全导航到个人主页（显式处理未登录）
func (s *SavedContentAction) safeNavigateToProfile(ctx context.Context) (string, error) {
	page := s.page.Context(ctx)

	if err := rod.Try(func() {
		page.MustNavigate("https://www.xiaohongshu.com/explore")
		page.MustWaitStable()
	}); err != nil {
		return "", fmt.Errorf("导航到首页失败: %w", err)
	}

	if err := checkLoginState(page); err != nil {
		return "", err
	}

	// 查找侧边栏 "我" 入口
	el, err := page.Timeout(5 * time.Second).Element(
		`div.main-container li.user.side-bar-component a.link-wrapper span.channel`,
	)
	if err != nil {
		return "", ErrNotLoggedIn
	}

	if err := el.Click(proto.InputMouseButtonLeft, 1); err != nil {
		return "", fmt.Errorf("点击用户入口失败: %w", err)
	}

	if err := rod.Try(func() {
		page.MustWaitStable()
	}); err != nil {
		return "", fmt.Errorf("等待个人主页加载失败: %w", err)
	}

	// 等待 URL 切换到个人主页（/user/profile/），避免在 SPA 路由切换完成前读取 URL
	if err := s.waitForProfileURL(page); err != nil {
		return "", err
	}

	if err := checkLoginState(page); err != nil {
		return "", err
	}

	info, err := page.Info()
	if err != nil {
		return "", fmt.Errorf("获取页面信息失败: %w", err)
	}

	logrus.Infof("已导航到个人主页: %s", info.URL)
	return info.URL, nil
}

// clickBoardSubTab 点击"专辑" subTab 触发收藏夹数据加载
func (s *SavedContentAction) clickBoardSubTab(page *rod.Page) error {
	el, err := page.Timeout(5*time.Second).ElementR("span", "专辑")
	if err != nil {
		return fmt.Errorf("未找到专辑 subTab 元素: %w", err)
	}

	if err := el.Click(proto.InputMouseButtonLeft, 1); err != nil {
		return fmt.Errorf("点击专辑 subTab 失败: %w", err)
	}

	_ = page.WaitStable(2 * time.Second)
	logrus.Info("已点击专辑 subTab")
	return nil
}

// waitForBoardList 等待专辑数据加载（boardPageStatus 变为非 pending）
func (s *SavedContentAction) waitForBoardList(page *rod.Page) error {
	const maxAttempts = 15
	for i := 0; i < maxAttempts; i++ {
		// 检查 boardPageStatus 是否已变为 resolved（数据加载完成）
		status := page.MustEval(readBoardPageStatusJS).String()
		if status != "" && status != "pending" {
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	// 超时后仍为 pending，但不阻塞 — 后续读取会处理空数据
	logrus.Warn("等待专辑数据加载超时，boardPageStatus 仍为 pending")
	return nil
}

// waitForProfileURL 轮询等待 URL 切换到个人主页，同时检测登录弹窗
func (s *SavedContentAction) waitForProfileURL(page *rod.Page) error {
	const maxAttempts = 10
	for i := 0; i < maxAttempts; i++ {
		// 优先检测登录弹窗（点击后可能触发登录而非跳转）
		if err := checkLoginState(page); err != nil {
			return err
		}
		info, err := page.Info()
		if err != nil {
			return fmt.Errorf("获取页面信息失败: %w", err)
		}
		if strings.Contains(info.URL, "/user/profile/") {
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("等待个人主页 URL 超时，可能未成功导航")
}

// ========== 数据读取 ==========

// readCollections 从 board.userBoardList._value 读取收藏夹
func (s *SavedContentAction) readCollections(page *rod.Page) ([]Collection, error) {
	raw := page.MustEval(readCollectionsJS).String()
	if raw == "" {
		return nil, fmt.Errorf("无法读取收藏夹数据，state key 未匹配")
	}

	var collections []Collection
	if err := json.Unmarshal([]byte(raw), &collections); err != nil {
		return nil, fmt.Errorf("收藏夹数据格式不匹配: %w", err)
	}
	return collections, nil
}

// readSavedFeeds 从 user.notes._value 读取收藏笔记
func (s *SavedContentAction) readSavedFeeds(page *rod.Page) ([]Feed, error) {
	raw := page.MustEval(readSavedFeedsJS).String()
	if raw == "" {
		logrus.Warn("未找到收藏内容数据")
		return nil, fmt.Errorf("无法读取收藏数据，state key 未匹配")
	}
	return flattenFeeds([]byte(raw))
}

// readBoardNotes 从 board.boardFeedsMap._value[boardId].notes 读取专辑笔记
func (s *SavedContentAction) readBoardNotes(page *rod.Page, boardID string) ([]BoardNote, error) {
	expr := readBoardNotesExpr(boardID)
	raw := page.MustEval(expr).String()
	if raw == "" {
		logrus.Warn("未找到专辑内容数据")
		return nil, fmt.Errorf("无法读取专辑数据")
	}

	var notes []BoardNote
	if err := json.Unmarshal([]byte(raw), &notes); err != nil {
		return nil, fmt.Errorf("专辑数据格式不匹配: %w", err)
	}
	return notes, nil
}

// flattenFeeds 归一化 Feed 数据（处理 []Feed 和 [][]Feed）
func flattenFeeds(raw []byte) ([]Feed, error) {
	var feeds []Feed
	if err := json.Unmarshal(raw, &feeds); err == nil {
		return feeds, nil
	}

	var nestedFeeds [][]Feed
	if err := json.Unmarshal(raw, &nestedFeeds); err == nil {
		var result []Feed
		for _, group := range nestedFeeds {
			result = append(result, group...)
		}
		return result, nil
	}

	return nil, fmt.Errorf("无法解析收藏数据格式")
}

// ========== 翻页 ==========

// scrollAndCollectCollections 滚动加载更多收藏夹
func (s *SavedContentAction) scrollAndCollectCollections(page *rod.Page, initial []Collection, limit int) []Collection {
	seen := make(map[string]bool, len(initial))
	var all []Collection
	for _, c := range initial {
		if !seen[c.ID] {
			seen[c.ID] = true
			all = append(all, c)
		}
	}

	if len(all) >= limit {
		return all[:limit]
	}

	const maxStale = 5
	staleCount := 0

	for staleCount < maxStale && len(all) < limit {
		// 使用 JS 滚动触发懒加载（比 Mouse.Scroll 更可靠，与 search_pagination 保持一致）
		if _, err := page.Eval(`() => window.scrollBy(0, 1500)`); err != nil {
			break
		}
		time.Sleep(2 * time.Second)
		_ = page.WaitStable(1 * time.Second)

		raw := page.MustEval(readCollectionsJS).String()
		if raw == "" {
			staleCount++
			continue
		}

		var collections []Collection
		if err := json.Unmarshal([]byte(raw), &collections); err != nil {
			staleCount++
			continue
		}

		newCount := 0
		for _, c := range collections {
			if !seen[c.ID] {
				seen[c.ID] = true
				all = append(all, c)
				newCount++
			}
		}

		if newCount == 0 {
			staleCount++
		} else {
			staleCount = 0
		}

		logrus.Infof("收藏夹翻页: 新增 %d 个, 总计 %d/%d", newCount, len(all), limit)
	}

	if len(all) > limit {
		return all[:limit]
	}
	return all
}

// scrollAndCollectFeeds 滚动加载更多收藏笔记
func (s *SavedContentAction) scrollAndCollectFeeds(page *rod.Page, initial []Feed, limit int) []Feed {
	seen := make(map[string]bool, len(initial))
	var all []Feed
	for _, f := range initial {
		if !seen[f.ID] {
			seen[f.ID] = true
			all = append(all, f)
		}
	}

	if len(all) >= limit {
		return all[:limit]
	}

	const maxStale = 5
	staleCount := 0

	for staleCount < maxStale && len(all) < limit {
		// 使用 JS 滚动触发懒加载（比 Mouse.Scroll 更可靠，与 search_pagination 保持一致）
		if _, err := page.Eval(`() => window.scrollBy(0, 1500)`); err != nil {
			break
		}
		time.Sleep(2 * time.Second)
		_ = page.WaitStable(1 * time.Second)

		obj, err := page.Eval(readSavedFeedsJS)
		if err != nil {
			break
		}

		raw := obj.Value.String()
		if raw == "" {
			staleCount++
			continue
		}

		feeds, err := flattenFeeds([]byte(raw))
		if err != nil {
			staleCount++
			continue
		}

		newCount := 0
		for _, f := range feeds {
			if !seen[f.ID] {
				seen[f.ID] = true
				all = append(all, f)
				newCount++
			}
		}

		if newCount == 0 {
			staleCount++
		} else {
			staleCount = 0
		}

		logrus.Infof("收藏翻页: 新增 %d 条, 总计 %d/%d", newCount, len(all), limit)
	}

	if len(all) > limit {
		return all[:limit]
	}
	return all
}

// scrollAndCollectBoardNotes 滚动加载更多专辑笔记
func (s *SavedContentAction) scrollAndCollectBoardNotes(page *rod.Page, boardID string, initial []BoardNote, limit int) []BoardNote {
	seen := make(map[string]bool, len(initial))
	var all []BoardNote
	for _, n := range initial {
		if !seen[n.NoteID] {
			seen[n.NoteID] = true
			all = append(all, n)
		}
	}

	if len(all) >= limit {
		return all[:limit]
	}

	const maxStale = 5
	staleCount := 0
	expr := readBoardNotesExpr(boardID)

	for staleCount < maxStale && len(all) < limit {
		// 使用 JS 滚动触发懒加载（比 Mouse.Scroll 更可靠，与 search_pagination 保持一致）
		if _, err := page.Eval(`() => window.scrollBy(0, 1500)`); err != nil {
			break
		}
		time.Sleep(2 * time.Second)
		_ = page.WaitStable(1 * time.Second)

		obj, err := page.Eval(expr)
		if err != nil {
			break
		}

		raw := obj.Value.String()
		if raw == "" {
			staleCount++
			continue
		}

		var notes []BoardNote
		if err := json.Unmarshal([]byte(raw), &notes); err != nil {
			staleCount++
			continue
		}

		newCount := 0
		for _, n := range notes {
			if !seen[n.NoteID] {
				seen[n.NoteID] = true
				all = append(all, n)
				newCount++
			}
		}

		if newCount == 0 {
			staleCount++
		} else {
			staleCount = 0
		}

		logrus.Infof("专辑翻页: 新增 %d 条, 总计 %d/%d", newCount, len(all), limit)
	}

	if len(all) > limit {
		return all[:limit]
	}
	return all
}

// ========== 登录检测 ==========

// checkLoginState 多信号登录检测
func checkLoginState(page *rod.Page) error {
	if info, err := page.Info(); err == nil {
		url := info.URL
		if strings.Contains(url, "/login") || strings.Contains(url, "signin") {
			return ErrNotLoggedIn
		}
	}

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

// ========== JS 表达式 ==========

// readBoardPageStatusJS 读取 board.boardPageStatus（pending/resolved）
const readBoardPageStatusJS = `() => {
	const state = window.__INITIAL_STATE__;
	if (!state || !state.board || !state.board.boardPageStatus) return "";
	return state.board.boardPageStatus;
}`

// readCollectionsJS 读取收藏夹列表: board.userBoardList._value
// 返回 "" 表示 key 未命中，"[]" 表示空数组（正常无收藏夹）
const readCollectionsJS = `() => {
	const state = window.__INITIAL_STATE__;
	if (!state || !state.board || !state.board.userBoardList) return "";
	const data = state.board.userBoardList._value || state.board.userBoardList.value;
	if (!Array.isArray(data)) return "";
	return JSON.stringify(data);
}`

// readSavedFeedsJS 读取收藏笔记: user.notes._value
const readSavedFeedsJS = `() => {
	const state = window.__INITIAL_STATE__;
	if (!state || !state.user || !state.user.notes) return "";
	const data = state.user.notes._value || state.user.notes.value;
	if (!data) return "";
	return JSON.stringify(data);
}`

// readBoardNotesExpr 读取专辑笔记: board.boardFeedsMap._value[boardId].notes
// boardID 已通过 validBoardID 校验，安全拼接
func readBoardNotesExpr(boardID string) string {
	return fmt.Sprintf(`() => {
		const state = window.__INITIAL_STATE__;
		if (!state || !state.board || !state.board.boardFeedsMap) return "";
		const feedsMap = state.board.boardFeedsMap._value || state.board.boardFeedsMap.value;
		if (!feedsMap) return "";
		const entry = feedsMap["%s"];
		if (!entry || !entry.notes) return "";
		return JSON.stringify(entry.notes);
	}`, boardID)
}
