package xiaohongshu

import (
	"context"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/xpzouying/xiaohongshu-mcp/browser"
	"github.com/xpzouying/xiaohongshu-mcp/configs"
)

// TestGetCollectionContentIntegration 真机集成测试（非 CI；需登录 cookies + 网络）。
// 验证 get_collection_content 可靠枚举所有可服务笔记，连续多次不空不卡。
//
// 默认 headed（匹配生产 -headless=false；headless 模式下 board 页渲染退化）。
// 运行示例（claude 板连跑 3 次）：
//
//	COOKIES_PATH=/Users/xiaorongli/.config/xiaohongshu-mcp/cookies.json \
//	XHS_BASE_URL=https://www.rednote.com \
//	XHS_COLLECTION_INTEGRATION=1 XHS_COLLECTION_BOARD=6884765b0000000023036a58 \
//	XHS_COLLECTION_LIMIT=200 XHS_COLLECTION_RUNS=3 \
//	go test ./xiaohongshu -run TestGetCollectionContentIntegration -v -timeout 900s
func TestGetCollectionContentIntegration(t *testing.T) {
	if os.Getenv("XHS_COLLECTION_INTEGRATION") == "" {
		t.Skip("set XHS_COLLECTION_INTEGRATION=1 to run the real-browser integration test")
	}
	boardID := os.Getenv("XHS_COLLECTION_BOARD")
	if boardID == "" {
		t.Fatal("XHS_COLLECTION_BOARD required")
	}
	limit := envInt("XHS_COLLECTION_LIMIT", 200)
	runs := envInt("XHS_COLLECTION_RUNS", 1)
	// 默认 headed（匹配生产）；XHS_COLLECTION_HEADLESS=1 才用 headless。
	headless := os.Getenv("XHS_COLLECTION_HEADLESS") == "1"

	t.Logf("integration: board=%s limit=%d runs=%d headless=%v baseURL=%s",
		boardID, limit, runs, headless, configs.BaseURL())

	for run := 1; run <= runs; run++ {
		runOnce(t, run, boardID, limit, headless)
	}
}

func runOnce(t *testing.T, run int, boardID string, limit int, headless bool) {
	t.Helper()
	b := browser.NewBrowser(headless)
	defer b.Close()
	page := b.NewPage()
	defer page.Close()

	action := NewSavedContentAction(page)
	start := time.Now()
	notes, total, err := action.GetCollectionContent(context.Background(), boardID, limit)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("run %d: error after %s: %v", run, elapsed, err)
	}

	t.Logf("run %d: count=%d total=%d elapsed=%s", run, len(notes), total, elapsed)

	// 验收：非空、不卡（elapsed 远低于旧版 WaitStable 的分钟级）。
	if len(notes) == 0 {
		t.Fatalf("run %d: got 0 notes (board total=%d) — returned empty", run, total)
	}
	// 每条都含 noteId + xsecToken（下游 get_feed_detail 必需）。
	for i, n := range notes {
		if n.NoteID == "" {
			t.Fatalf("run %d: note[%d] missing noteId", run, i)
		}
		if n.XsecToken == "" {
			t.Fatalf("run %d: note[%d] (%s) missing xsecToken", run, i, n.NoteID)
		}
	}
	// 全部唯一（dedup 生效）。
	seen := make(map[string]bool, len(notes))
	for _, n := range notes {
		if seen[n.NoteID] {
			t.Fatalf("run %d: duplicate noteId %s", run, n.NoteID)
		}
		seen[n.NoteID] = true
	}

	// 可选精确验收断言（设了才检）：覆盖本 issue 的核心数值。
	if want := envInt("XHS_COLLECTION_EXPECT_TOTAL", -1); want >= 0 && total != want {
		t.Fatalf("run %d: total=%d, want %d", run, total, want)
	}
	if want := envInt("XHS_COLLECTION_EXPECT_COUNT", -1); want >= 0 && len(notes) != want {
		t.Fatalf("run %d: count=%d, want %d", run, len(notes), want)
	}
}

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}
