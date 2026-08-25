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
	"github.com/xpzouying/xiaohongshu-mcp/configs"
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
	page := s.page.Context(ctx).Timeout(180 * time.Second)

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

	// RedNote 页面不一定展示中文"专辑" subTab，先尝试 URL 直达，再回退到 DOM 点击。
	if err := s.loadBoardSubTab(page, favURL); err != nil {
		return nil, fmt.Errorf("加载专辑 subTab 失败: %w", err)
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

// GetCollectionContent 获取指定专辑中的笔记。
//
// 可靠枚举所有“可服务”笔记：以 board 页 __INITIAL_STATE__ 里
// boardFeedsMap[id].hasMore 作为平台权威结束信号（hasMore=false 即到底），
// 不再依赖盲目的 maxStale 停滞计数。返回 notes、board total（来自 boardDetails，
// 0 表示未知）、error。注意 total 可能 > 可服务数（已删除/不可见笔记仍计入 total
// 但 feed 不返回），调用方据 count vs total 看到差额。
func (s *SavedContentAction) GetCollectionContent(ctx context.Context, collectionID string, limit int) ([]BoardNote, int, error) {
	if !validBoardID.MatchString(collectionID) {
		return nil, 0, fmt.Errorf("无效的专辑 ID: %s", collectionID)
	}

	// Context(ctx) 用 request ctx 替换 constructor 里 page.Timeout(180s) 的 deadline ctx。
	// 之后所有 op(d) 现起的 Timeout(d) clone 都派生自 ctx，**不继承 180s 总预算** ——
	// 否则大收藏夹的整条滚动循环会被一个累计 180s 总预算掐死（rod 的 page.Timeout
	// 是累计总预算，非 per-call）。故 board 枚举路径用 per-op 短超时。
	page := s.page.Context(ctx)
	op := func(d time.Duration) *rod.Page { return page.Timeout(d) }

	// 专辑页使用独立 URL，无需经过个人主页
	boardURL := fmt.Sprintf(configs.BaseURL()+"/board/%s", collectionID)
	logrus.Infof("导航到专辑: %s", boardURL)

	// 故意不用 MustWaitStable —— board 页图片/视频懒加载永不 stable，
	// WaitStable 会一直等到 page 超时（实测 35-60s）。
	if err := rod.Try(func() {
		op(40 * time.Second).MustNavigate(boardURL)
	}); err != nil {
		return nil, 0, fmt.Errorf("导航到专辑页面失败: %w", err)
	}

	// 登录态检测（专辑页不经过 safeNavigateToProfile，需单独检查）
	if err := checkLoginState(page); err != nil {
		return nil, 0, err
	}

	// 不可访问检测（删除/私密/违规）—— checkPageAccessible 自带 2s 超时，非 WaitStable。
	if err := checkPageAccessible(page); err != nil {
		return nil, 0, fmt.Errorf("专辑不存在或无法访问: %w", err)
	}

	if err := rod.Try(func() {
		op(20 * time.Second).MustWait(`() => window.__INITIAL_STATE__ !== undefined`)
	}); err != nil {
		return nil, 0, fmt.Errorf("等待专辑页面状态失败: %w", err)
	}

	// 首屏 retry：poll 直到 boardFeedsMap[id] entry 出现（或确认真空板），修偶发返空。
	first, err := s.waitForBoardFirstScreen(ctx, op, collectionID)
	if err != nil {
		return nil, 0, err
	}

	collector := newBoardCollector(limit)
	collector.add(first.Notes)

	reason := boardStopReason(collector, first)
	if reason == stopContinue {
		reason, err = s.scrollAndCollectBoardNotes(ctx, op, collectionID, collector, first)
		if err != nil {
			return nil, 0, err // ctx 取消/超时
		}
	}

	// total 通常首屏即在（boardDetails 与 boardFeedsMap 同时加载）；若首屏未读到，
	// 枚举结束后再读一次兜底（此时 boardDetails 必已加载）。
	total := first.Total
	if !first.TotalKnown {
		if fin := s.readBoardStateSafe(op, collectionID); fin.TotalKnown {
			total = fin.Total
		}
	}

	notes := collector.result()
	logrus.Infof("专辑 %s 枚举完成: count=%d total=%d stop=%s",
		collectionID, len(notes), total, reason)
	return notes, total, nil
}

// ListSavedContent 获取全部收藏内容
func (s *SavedContentAction) ListSavedContent(ctx context.Context, limit int) ([]Feed, error) {
	page := s.page.Context(ctx).Timeout(180 * time.Second)

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
	page := s.page.Context(ctx).Timeout(180 * time.Second)

	if err := rod.Try(func() {
		page.MustNavigate(configs.BaseURL() + "/explore")
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

// loadBoardSubTab 加载"专辑/收藏夹" subTab 触发收藏夹数据加载
func (s *SavedContentAction) loadBoardSubTab(page *rod.Page, favURL string) error {
	boardURL := appendSubTabToURL(favURL, "board")
	logrus.Infof("尝试直接导航到专辑 subTab: %s", boardURL)
	// readCollectionsJS 的 MustEval 必须在 rod.Try 内：导航成功但 eval 因 context/
	// JS execution context 失败时应回退到 DOM 点击，而非 panic。
	var raw string
	if err := rod.Try(func() {
		page.MustNavigate(boardURL)
		page.MustWaitStable()
		page.MustWait(`() => window.__INITIAL_STATE__ !== undefined`)
		raw = page.MustEval(readCollectionsJS).String()
	}); err == nil && raw != "" {
		logrus.Info("已通过 URL 直达加载专辑 subTab")
		return nil
	}

	return s.clickBoardSubTab(page)
}

// clickBoardSubTab 点击"专辑/收藏夹" subTab 触发收藏夹数据加载
func (s *SavedContentAction) clickBoardSubTab(page *rod.Page) error {
	labels := []string{"专辑", "收藏夹", "Collections", "Collection", "Boards", "Board"}
	var lastErr error
	for _, label := range labels {
		el, err := page.Timeout(2*time.Second).ElementR("span", label)
		if err != nil {
			lastErr = err
			continue
		}

		if err := el.Click(proto.InputMouseButtonLeft, 1); err != nil {
			lastErr = err
			continue
		}

		_ = page.WaitStable(2 * time.Second)
		logrus.Infof("已点击专辑 subTab: %s", label)
		return nil
	}

	return fmt.Errorf("未找到专辑 subTab 元素: %w", lastErr)
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

// boardState 是一次 board state 读取的结构化结果（三态语义：用 *Known 区分
// “字段缺失/未加载” 与 “明确值”，避免把 falsey JS 值坍缩成 “到底/空”）。
type boardState struct {
	EntryPresent bool        `json:"entryPresent"` // boardFeedsMap[id] entry 是否已加载
	Notes        []BoardNote `json:"notes"`        // entry.notes（仅 entryPresent && 数组时有效）
	HasMore      bool        `json:"hasMore"`
	HasMoreKnown bool        `json:"hasMoreKnown"` // entry.hasMore 字段是否存在（boolean）
	Cursor       string      `json:"cursor"`
	Total        int         `json:"total"`      // boardDetails[id].total（saved 计数，含已删除）
	TotalKnown   bool        `json:"totalKnown"` // boardDetails total 是否读到
}

// readBoardStateSafe 读取结构化 board state，绝不 panic：eval 失败/空数据
// 返回零值 boardState（EntryPresent=false），由调用方按“未加载，继续轮询”处理。
func (s *SavedContentAction) readBoardStateSafe(op func(time.Duration) *rod.Page, boardID string) boardState {
	var raw string
	if err := rod.Try(func() {
		raw = op(8 * time.Second).MustEval(readBoardStateExpr(boardID)).String()
	}); err != nil {
		logrus.Debugf("读取专辑 state 失败（将继续轮询）: %v", err)
		return boardState{}
	}
	if raw == "" {
		return boardState{}
	}
	var st boardState
	if err := json.Unmarshal([]byte(raw), &st); err != nil {
		logrus.Debugf("专辑 state 解析失败: %v", err)
		return boardState{}
	}
	return st
}

// waitForBoardFirstScreen poll 首屏直到 entry 出现（拿到 notes 或确认真空板）。
// 修偶发“返空”：首屏数据未加载时不再直接报错，而是有界轮询。
func (s *SavedContentAction) waitForBoardFirstScreen(ctx context.Context, op func(time.Duration) *rod.Page, boardID string) (boardState, error) {
	deadline := time.Now().Add(15 * time.Second)
	var st boardState
	for time.Now().Before(deadline) {
		if err := ctx.Err(); err != nil {
			return boardState{}, err
		}
		st = s.readBoardStateSafe(op, boardID)
		// ready = entry 在 且 (已有 notes 或 已确认到底=真空板)
		if st.EntryPresent && (len(st.Notes) > 0 || (st.HasMoreKnown && !st.HasMore)) {
			return st, nil
		}
		if err := sleepCtx(ctx, 400*time.Millisecond); err != nil {
			return boardState{}, err
		}
	}
	if st.EntryPresent {
		// entry 在但首屏未拿到 notes 且未确认到底 —— 交给上层（通常会进入滚动轮询）。
		return st, nil
	}
	keys := s.readBoardKeys(op)
	return boardState{}, fmt.Errorf("无法读取专辑 %s 数据：首屏 entry 未加载（board keys=%s）", boardID, keys)
}

// pollBoardGrowth 滚动后轮询：等 notes 增长 / 确认到底 / poll 预算耗尽，返回最新 state。
// 替代固定 sleep + WaitStable（后者在 board 页会卡 35-60s）。ctx 取消则立即返回 ctx.Err()。
func (s *SavedContentAction) pollBoardGrowth(ctx context.Context, op func(time.Duration) *rod.Page, boardID string, prevLen int) (boardState, error) {
	deadline := time.Now().Add(8 * time.Second)
	var st boardState
	for {
		st = s.readBoardStateSafe(op, boardID)
		if len(st.Notes) > prevLen {
			return st, nil // 增长
		}
		if st.EntryPresent && st.HasMoreKnown && !st.HasMore {
			return st, nil // 平台到底
		}
		if !time.Now().Before(deadline) {
			return st, nil // 预算耗尽（stall 或读取失败，由上层判定）
		}
		if err := sleepCtx(ctx, 400*time.Millisecond); err != nil {
			return st, err
		}
	}
}

// sleepCtx 可被 ctx 取消打断的 sleep（调用方取消/请求断开/shutdown 时立即返回）。
func sleepCtx(ctx context.Context, d time.Duration) error {
	select {
	case <-time.After(d):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// readBoardKeys 仅用于错误诊断：读 __INITIAL_STATE__.board 的 key 列表（有界）。
func (s *SavedContentAction) readBoardKeys(op func(time.Duration) *rod.Page) string {
	var keys string
	_ = rod.Try(func() {
		keys = op(5 * time.Second).MustEval(
			`() => { try { return JSON.stringify(Object.keys(window.__INITIAL_STATE__?.board || {})); } catch (e) { return ""; } }`,
		).String()
	})
	return keys
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

// scrollAndCollectBoardNotes 以 hasMore 为权威停止信号滚动枚举专辑笔记。
//
// 每轮：scrollTo 到真实底部 → 计数轮询（pollBoardGrowth，去 WaitStable）→ dedup 累积。
// 停止：limit 收够 / 平台到底（hasMore=false）/ stall 安全网。
// stall 安全网（防误停 & 防死循环）：仅“成功读到 hasMore=true 且 notes 未增长”
// 才计为 stall；eval 失败/unknown 不当到底。stall 达阈值后做加力滚动 + final read，
// 仍不增长才放弃。
func (s *SavedContentAction) scrollAndCollectBoardNotes(
	ctx context.Context, op func(time.Duration) *rod.Page, boardID string, c *boardCollector, first boardState,
) (stopReason, error) {
	const (
		maxIterations = 60 // 安全上限：防 hasMore 永远 true 又不增长时死循环
		maxStall      = 6
	)
	stall := 0
	prevLen := len(first.Notes)

	for i := 0; i < maxIterations; i++ {
		if err := ctx.Err(); err != nil {
			return stopContinue, err
		}
		_ = rod.Try(func() { op(8 * time.Second).MustEval(scrollBoardJS) })

		st, err := s.pollBoardGrowth(ctx, op, boardID, prevLen)
		if err != nil {
			return stopContinue, err // ctx 取消
		}
		c.add(st.Notes)
		if reason := boardStopReason(c, st); reason != stopContinue {
			return reason, nil
		}

		grew := len(st.Notes) > prevLen
		if grew {
			prevLen = len(st.Notes)
		}

		// 仅“读取成功 + hasMore=true + 未增长”才算真 stall
		if !grew && st.EntryPresent && st.HasMoreKnown && st.HasMore {
			stall++
			if stall >= maxStall {
				// 加力滚动序列 + final read：给懒加载最后的机会
				for j := 0; j < 3; j++ {
					_ = rod.Try(func() { op(8 * time.Second).MustEval(scrollBoardJS) })
					if err := sleepCtx(ctx, 1500*time.Millisecond); err != nil {
						return stopContinue, err
					}
				}
				stFinal := s.readBoardStateSafe(op, boardID)
				c.add(stFinal.Notes)
				if reason := boardStopReason(c, stFinal); reason != stopContinue {
					return reason, nil
				}
				logrus.Warnf("专辑 %s stall 放弃: count=%d limit=%d hasMore=%v cursor=%s stalls=%d",
					boardID, c.count(), c.limit, stFinal.HasMore, stFinal.Cursor, stall)
				return stopStall, nil
			}
		} else {
			stall = 0
		}

		logrus.Infof("专辑翻页: count=%d total=%d hasMore=%v", c.count(), st.Total, st.HasMore)
	}

	logrus.Warnf("专辑 %s 达迭代上限 %d: count=%d", boardID, maxIterations, c.count())
	return stopStall, nil
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

// appendSubTabToURL 在 URL 上追加 subTab 参数
func appendSubTabToURL(pageURL, subTab string) string {
	if strings.Contains(pageURL, "?") {
		return pageURL + "&subTab=" + subTab
	}
	return pageURL + "?subTab=" + subTab
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

// scrollBoardJS 滚到页面真实底部触发懒加载（实测 headed 模式下 window 滚动驱动加载，
// 优于固定 scrollBy(0,1500)：页面随内容增长，scrollTo 始终能抵达触发区）。
const scrollBoardJS = `() => {
	window.scrollTo(0, document.body.scrollHeight);
	// 防御：若页面改用内层容器滚动，找最大的可滚动容器一并滚到底。
	let best = null, bh = 0;
	document.querySelectorAll('div,section,main').forEach(el => {
		if (el.scrollHeight > el.clientHeight + 100 && el.clientHeight > 300 && el.scrollHeight > bh) {
			bh = el.scrollHeight; best = el;
		}
	});
	if (best) { best.scrollTop = best.scrollHeight; }
}`

// readBoardStateExpr 一次读取结构化 board state（notes + hasMore + cursor + total），
// 三态语义：字段缺失/类型不符时对应 *Known=false。boardID 已通过 validBoardID 校验。
// 全程空值兜底 + try/catch，绝不抛错（go-rod 侧再包 rod.Try）。
//   - 笔记/翻页: board.boardFeedsMap._value[id] = {cursor, hasMore, notes}
//   - total:     board.boardDetails._value[id].total（saved 计数，含已删除/不可见）
func readBoardStateExpr(boardID string) string {
	return fmt.Sprintf(`() => {
		const out = {entryPresent:false, notes:[], hasMore:false, hasMoreKnown:false, cursor:"", total:0, totalKnown:false};
		try {
			const state = window.__INITIAL_STATE__;
			if (!state || !state.board) return JSON.stringify(out);
			const board = state.board;
			const fmRaw = board.boardFeedsMap;
			const fm = fmRaw ? (fmRaw._value || fmRaw.value || fmRaw) : null;
			const entry = fm ? fm["%s"] : null;
			if (entry) {
				out.entryPresent = true;
				if (Array.isArray(entry.notes)) out.notes = entry.notes;
				if (typeof entry.hasMore === 'boolean') { out.hasMore = entry.hasMore; out.hasMoreKnown = true; }
				if (typeof entry.cursor === 'string') out.cursor = entry.cursor;
			}
			const bdRaw = board.boardDetails;
			const bm = bdRaw ? (bdRaw._value || bdRaw.value || bdRaw) : null;
			// boardDetails 解包后通常就是 detail 对象本身（{total,id,name,...}，非按 id 索引的 map）；
			// 个别情况可能按 id 索引，故 bm[id] 优先、回退到 bm 本身。
			const d = bm ? (bm["%s"] || bm) : null;
			if (d && typeof d.total === 'number') { out.total = d.total; out.totalKnown = true; }
		} catch (e) {}
		return JSON.stringify(out);
	}`, boardID, boardID)
}

// ========== 纯逻辑：枚举累积 + 终止判定（无浏览器，可单测） ==========

// stopReason 描述枚举停止原因。
type stopReason string

const (
	stopContinue     stopReason = ""              // 继续
	stopLimitReached stopReason = "limit_reached" // 收够 limit
	stopHasMoreFalse stopReason = "hasMore_false" // 平台权威到底
	stopStall        stopReason = "stall"         // 安全网放弃
)

// boardCollector 按 NoteID dedup 累积笔记并按 limit 截断。limit<=0 表示不限。
type boardCollector struct {
	limit int
	seen  map[string]bool
	all   []BoardNote
}

func newBoardCollector(limit int) *boardCollector {
	return &boardCollector{limit: limit, seen: make(map[string]bool)}
}

// add 合并一批 notes（去重、跳过空 NoteID），返回新增数量。
func (c *boardCollector) add(notes []BoardNote) int {
	added := 0
	for _, n := range notes {
		if n.NoteID == "" || c.seen[n.NoteID] {
			continue
		}
		c.seen[n.NoteID] = true
		c.all = append(c.all, n)
		added++
	}
	return added
}

func (c *boardCollector) count() int { return len(c.all) }

// reachedLimit 是否已收够（limit<=0 永远 false）。
func (c *boardCollector) reachedLimit() bool {
	return c.limit > 0 && len(c.all) >= c.limit
}

// result 返回结果，按 limit 截断。
func (c *boardCollector) result() []BoardNote {
	if c.limit > 0 && len(c.all) > c.limit {
		return c.all[:c.limit]
	}
	return c.all
}

// boardStopReason 依据当前累积 + 最近一次 state 读取判定权威停止。
// 仅 EntryPresent && HasMoreKnown && !HasMore 才是平台到底；entry 缺失 / hasMore
// 未知都返回 stopContinue（继续轮询，绝不把未加载/改版误判成空）。limit 优先。
func boardStopReason(c *boardCollector, st boardState) stopReason {
	if c.reachedLimit() {
		return stopLimitReached
	}
	if st.EntryPresent && st.HasMoreKnown && !st.HasMore {
		return stopHasMoreFalse
	}
	return stopContinue
}
