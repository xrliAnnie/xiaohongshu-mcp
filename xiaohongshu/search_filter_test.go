package xiaohongshu

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/proto"
	"github.com/stretchr/testify/require"
	"github.com/xpzouying/xiaohongshu-mcp/browser"
)

func TestCollectFilters(t *testing.T) {
	t.Run("默认值不点", func(t *testing.T) {
		pending, err := collectFilters([]FilterOption{{SortBy: "综合", NoteType: "不限", PublishTime: "不限"}})
		require.NoError(t, err)
		require.Empty(t, pending)
	})

	t.Run("按组顺序输出，同组以最后一个非空值为准", func(t *testing.T) {
		pending, err := collectFilters([]FilterOption{
			{PublishTime: "一天内", SortBy: "最新"},
			{SortBy: "最多收藏"},
			{SortBy: ""},
		})
		require.NoError(t, err)
		require.Equal(t, []pendingFilter{
			{group: "排序依据", option: "最多收藏"},
			{group: "发布时间", option: "一天内"},
		}, pending)
	})

	t.Run("全部五组", func(t *testing.T) {
		pending, err := collectFilters([]FilterOption{{
			SortBy: "最新", NoteType: "视频", PublishTime: "一周内", SearchScope: "已关注", Location: "同城",
		}})
		require.NoError(t, err)
		require.Len(t, pending, 5)
	})

	t.Run("写错的值在打开页面前就被拒绝，并列出可选值", func(t *testing.T) {
		_, err := collectFilters([]FilterOption{{NoteType: "不存在的类型"}})
		require.Error(t, err)
		require.Contains(t, err.Error(), "笔记类型")
		require.Contains(t, err.Error(), "不存在的类型")
		require.Contains(t, err.Error(), "视频")
	})
}

// fakeFilterPanel 复刻 2026-11 实测的筛选面板：每个选项除真实节点（data-hp-bound）外，
// 还有一份几乎透明的诱饵节点（button-hp-installed、aria-hidden、opacity 1e-05、z-index -1），
// 二者在 DOM 里交错排列。诱饵被点中会记为 honeypot。
const fakeFilterPanel = `<!doctype html><html><body>
<div class="filter">筛选</div>
<div class="filter-panel"><div class="filter-container"><div class="filters-wrapper">
%s
</div></div></div>
<script>
window.__clicks = [];
document.addEventListener("click", (e) => {
  const tag = e.target.closest("div.tags");
  if (!tag) return;
  window.__clicks.push((tag.dataset.hpKind ? "honeypot:" : "real:") + tag.textContent.trim());
}, true);
</script></body></html>`

const (
	honeypotNode = `<div class="tags%s" button-hp-installed="1" data-hp-kind="filter-tag-%s" aria-hidden="true" tabindex="-1" style="position:absolute;opacity:1e-05;pointer-events:auto;z-index:-1;width:96px;height:40px;left:%dpx;top:0"><span>%s</span></div>`
	realNode     = `<div class="tags%s" data-hp-bound="1" style="display:inline-flex;width:96px;height:40px;margin-right:12px;"><span>%s</span></div>`
)

// fakeGroup 生成一组：label + 每个选项一份诱饵 + 一份真实节点。
// honeypotFirst 控制诱饵放在真实节点前还是后。skipDecoy 里的选项没有诱饵（真机上"不限"就是这样）。
func fakeGroup(label string, options []string, active string, honeypotFirst bool, skipDecoy ...string) string {
	var b strings.Builder
	fmt.Fprintf(&b, `<div class="filters"><span>%s</span><div class="tag-container" style="position:relative;">`, label)
	for i, opt := range options {
		cls := ""
		if opt == active {
			cls = " active"
		}
		decoy := fmt.Sprintf(honeypotNode, cls, opt, i*108, opt)
		for _, s := range skipDecoy {
			if s == opt {
				decoy = ""
			}
		}
		real := fmt.Sprintf(realNode, cls, opt)
		if honeypotFirst {
			b.WriteString(decoy + real)
		} else {
			b.WriteString(real + decoy)
		}
	}
	b.WriteString(`</div></div>`)
	return b.String()
}

func newFilterPanelServer(t *testing.T, honeypotFirst bool) *httptest.Server {
	t.Helper()
	groups := strings.Join([]string{
		fakeGroup("排序依据", []string{"综合", "最新", "最多点赞", "最多评论", "最多收藏"}, "综合", honeypotFirst),
		fakeGroup("笔记类型", []string{"不限", "视频", "图文"}, "不限", honeypotFirst, "不限"),
		fakeGroup("发布时间", []string{"不限", "一天内", "一周内", "半年内"}, "不限", honeypotFirst, "不限"),
	}, "\n")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintf(w, fakeFilterPanel, groups)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestFindFilterOptionSkipsHoneypot 回归：旧实现用 div.tags:nth-child(N) 定位，
// 面板里选项成对出现后下标错位，"最多收藏"会落到"最多点赞"的诱饵节点上，点不动一直等到 60s 超时。
func TestFindFilterOptionSkipsHoneypot(t *testing.T) {
	if os.Getenv("XHS_BROWSER_TESTS") == "" {
		t.Skip("set XHS_BROWSER_TESTS=1 to run local-fixture browser tests (headless Chrome)")
	}
	t.Setenv("COOKIES_PATH", filepath.Join(t.TempDir(), "cookies.json"))

	for _, honeypotFirst := range []bool{true, false} {
		name := "诱饵在后"
		if honeypotFirst {
			name = "诱饵在前"
		}
		t.Run(name, func(t *testing.T) {
			srv := newFilterPanelServer(t, honeypotFirst)
			b := browser.NewBrowser(true)
			defer b.Close()
			page := b.NewPage()
			defer page.Close()
			page.MustNavigate(srv.URL).MustWaitLoad()

			for _, pf := range []pendingFilter{
				{group: "排序依据", option: "最多收藏"},
				{group: "发布时间", option: "一天内"},
				{group: "笔记类型", option: "图文"},
			} {
				opt, err := findFilterOption(page, pf)
				require.NoError(t, err, pf.option)
				require.NoError(t, opt.Click(proto.InputMouseButtonLeft, 1), pf.option)
			}

			clicks := page.MustEval(`() => window.__clicks.join(",")`).String()
			require.Equal(t, "real:最多收藏,real:一天内,real:图文", clicks)
		})
	}

	t.Run("找不到的组和选项给出明确错误", func(t *testing.T) {
		srv := newFilterPanelServer(t, true)
		b := browser.NewBrowser(true)
		defer b.Close()
		page := b.NewPage()
		defer page.Close()
		page.MustNavigate(srv.URL).MustWaitLoad()

		_, err := findFilterOption(page, pendingFilter{group: "位置距离", option: "同城"})
		require.ErrorContains(t, err, "位置距离")

		_, err = findFilterOption(page, pendingFilter{group: "排序依据", option: "最火"})
		require.ErrorContains(t, err, "最火")
		require.ErrorContains(t, err, "最多收藏", "应列出页面上实际有的选项")
	})

	t.Run("display:none 的节点即使不透明度为 1 也不可见", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			hidden := `<div class="filters"><span>排序依据</span><div class="tag-container">` +
				`<div class="tags" style="display:none"><span>最新</span></div></div></div>`
			fmt.Fprintf(w, fakeFilterPanel, hidden)
		}))
		defer srv.Close()
		b := browser.NewBrowser(true)
		defer b.Close()
		page := b.NewPage()
		defer page.Close()
		page.MustNavigate(srv.URL).MustWaitLoad()

		_, err := findFilterOption(page, pendingFilter{group: "排序依据", option: "最新"})
		require.ErrorContains(t, err, "不可见")
	})

	t.Run("只剩诱饵节点时报不可见，不点它", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			decoyOnly := `<div class="filters"><span>排序依据</span><div class="tag-container">` +
				fmt.Sprintf(honeypotNode, "", "最新", 0, "最新") + `</div></div>`
			fmt.Fprintf(w, fakeFilterPanel, decoyOnly)
		}))
		defer srv.Close()
		b := browser.NewBrowser(true)
		defer b.Close()
		page := b.NewPage()
		defer page.Close()
		page.MustNavigate(srv.URL).MustWaitLoad()

		_, err := findFilterOption(page, pendingFilter{group: "排序依据", option: "最新"})
		require.ErrorContains(t, err, "不可见")
	})
}

const fakeFeedsPage = `<!doctype html><html><body><script>
window.__INITIAL_STATE__ = {search: {feeds: {value: [{id: "a"}, {id: "b"}]}}};
window.change = () => {
  window.__INITIAL_STATE__.search.feeds.value = [];
  setTimeout(() => { window.__INITIAL_STATE__.search.feeds.value = [{id: "c"}, {id: "d"}]; }, 600);
};
</script></body></html>`

func newFeedsPage(t *testing.T) (*rod.Page, func()) {
	t.Helper()
	t.Setenv("COOKIES_PATH", filepath.Join(t.TempDir(), "cookies.json"))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, fakeFeedsPage)
	}))
	b := browser.NewBrowser(true)
	page := b.NewPage()
	page.MustNavigate(srv.URL).MustWaitLoad()
	return page, func() {
		_ = page.Close()
		b.Close()
		srv.Close()
	}
}

func TestWaitFeedsChanged(t *testing.T) {
	if os.Getenv("XHS_BROWSER_TESTS") == "" {
		t.Skip("set XHS_BROWSER_TESTS=1 to run local-fixture browser tests (headless Chrome)")
	}

	t.Run("等到新数据灌入，而不是读到清空的中间态", func(t *testing.T) {
		page, cleanup := newFeedsPage(t)
		defer cleanup()

		before := readFeedIDs(page)
		require.Equal(t, "a,b", before)
		page.MustEval(`() => window.change()`)
		require.True(t, waitFeedsChanged(page, before, 10*time.Second))
		require.Equal(t, "c,d", readFeedIDs(page))
	})

	t.Run("结果一直不变：到上限就返回 false", func(t *testing.T) {
		page, cleanup := newFeedsPage(t)
		defer cleanup()

		start := time.Now()
		require.False(t, waitFeedsChanged(page, readFeedIDs(page), 1*time.Second))
		require.Less(t, time.Since(start), 5*time.Second, "上限必须是硬的，不能被单次 Eval 拖长")
	})

	t.Run("调用方取消后立刻返回", func(t *testing.T) {
		page, cleanup := newFeedsPage(t)
		defer cleanup()

		ctx, cancel := context.WithCancel(context.Background())
		cancelled := page.Context(ctx)
		time.AfterFunc(500*time.Millisecond, cancel)

		start := time.Now()
		require.False(t, waitFeedsChanged(cancelled, readFeedIDs(page), 30*time.Second))
		require.Less(t, time.Since(start), 5*time.Second)
	})
}
