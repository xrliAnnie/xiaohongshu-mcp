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

	"github.com/stretchr/testify/require"
	"github.com/xpzouying/xiaohongshu-mcp/browser"
)

func TestRednoteDetailPath(t *testing.T) {
	path, err := rednoteDetailPath("6accdb19000000001a02aadf", "CBCO+a/b=")
	require.NoError(t, err)
	require.Equal(t, "/discovery/item/6accdb19000000001a02aadf?xsec_token=CBCO%2Ba%2Fb%3D&xsec_source=pc_feed", path)

	for _, bad := range []string{"", "../x", "6acc?x=1", "6acc/1", "6acc db"} {
		_, err := rednoteDetailPath(bad, "t")
		require.Error(t, err, "feedID %q 应被拒绝", bad)
	}
}

func TestRedactURL(t *testing.T) {
	require.Equal(t, "https://www.rednote.com/explore",
		redactURL("https://www.rednote.com/explore?xsec_token=secret&a=1"))
	require.Equal(t, "<unparseable>", redactURL("://bad"))
}

// fakeSPA 模拟 Rednote 新版前端：整页打开详情路由会被重定向到旧前端，
// 只有前端路由（router.push / pushState+popstate）进入详情才会写入 noteDetailMap。
const fakeSPA = `<!doctype html><html><body><div id="app"></div><script>
const MODE = %q;
function fill() {
  const m = location.pathname.match(/^\/discovery\/item\/([0-9a-z]+)$/);
  if (!m || MODE === "never") return;
  const id = m[1];
  const token = new URLSearchParams(location.search).get("xsec_token");
  window.__INITIAL_STATE__ = {note: {noteDetailMap: {
    "undefined": {note: {}, comments: {}},
    [id]: {note: {noteId: id, title: "t:" + token}, comments: {list: [], hasMore: false}}
  }}};
}
if (MODE === "router") {
  document.getElementById("app").__vue_app__ = {config: {globalProperties: {$router: {
    push(p) { history.pushState({}, "", p); setTimeout(fill, 300); }
  }}}};
}
addEventListener("popstate", () => setTimeout(fill, 300));
</script></body></html>`

func newFakeSPAServer(t *testing.T, mode string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.Redirect(w, r, "/legacy", http.StatusFound)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintf(w, fakeSPA, mode)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestOpenDetailInAppFixture(t *testing.T) {
	if os.Getenv("XHS_BROWSER_TESTS") == "" {
		t.Skip("set XHS_BROWSER_TESTS=1 to run local-fixture browser tests (headless Chrome)")
	}
	t.Setenv("COOKIES_PATH", filepath.Join(t.TempDir(), "cookies.json"))

	oldApp := appReadyTimeout
	appReadyTimeout = 2 * time.Second // popstate 场景没有 router，缩短等待
	t.Cleanup(func() { appReadyTimeout = oldApp })

	const id = "6accdb19000000001a02aadf"
	for _, mode := range []string{"router", "popstate"} {
		t.Run(mode, func(t *testing.T) {
			srv := newFakeSPAServer(t, mode)
			b := browser.NewBrowser(true)
			defer b.Close()
			page := b.NewPage()
			defer page.Close()

			require.NoError(t, openDetailInApp(page, srv.URL, id, "tok=="))
			resp, err := NewFeedDetailAction(page).extractFeedDetail(page, id)
			require.NoError(t, err)
			require.Equal(t, id, resp.Note.NoteID)
			require.Equal(t, "t:tok==", resp.Note.Title, "token 需原样经过前端路由")
		})
	}

	t.Run("state never appears", func(t *testing.T) {
		old := detailReadyTimeout
		detailReadyTimeout = 3 * time.Second
		t.Cleanup(func() { detailReadyTimeout = old })

		srv := newFakeSPAServer(t, "never")
		b := browser.NewBrowser(true)
		defer b.Close()
		page := b.NewPage()
		defer page.Close()

		start := time.Now()
		err := openDetailInApp(page, srv.URL, id, "secret-token")
		require.Error(t, err)
		require.Less(t, time.Since(start), 30*time.Second)
		require.Contains(t, err.Error(), srv.URL+"/discovery/item/"+id)
		require.NotContains(t, err.Error(), "secret-token")
	})
}

// TestGetFeedDetailRednoteIntegration 真机集成测试（非 CI；需登录 cookies + 网络，只读）。
//
//	COOKIES_PATH=/Users/xiaorongli/.config/xiaohongshu-mcp/cookies.json \
//	XHS_BASE_URL=https://www.rednote.com \
//	XHS_DETAIL_INTEGRATION=1 XHS_DETAIL_FEED_ID=<id> XHS_DETAIL_XSEC_TOKEN=<token> \
//	go test ./xiaohongshu -run TestGetFeedDetailRednoteIntegration -v -timeout 300s
func TestGetFeedDetailRednoteIntegration(t *testing.T) {
	if os.Getenv("XHS_DETAIL_INTEGRATION") == "" {
		t.Skip("set XHS_DETAIL_INTEGRATION=1 to run the real-browser integration test")
	}
	feedID, token := os.Getenv("XHS_DETAIL_FEED_ID"), os.Getenv("XHS_DETAIL_XSEC_TOKEN")
	require.NotEmpty(t, feedID, "XHS_DETAIL_FEED_ID required")
	require.NotEmpty(t, token, "XHS_DETAIL_XSEC_TOKEN required")

	b := browser.NewBrowser(os.Getenv("XHS_DETAIL_HEADED") != "1")
	defer b.Close()
	page := b.NewPage()
	defer page.Close()

	resp, err := NewFeedDetailAction(page).GetFeedDetail(context.Background(), feedID, token, false, DefaultCommentLoadConfig())
	require.NoError(t, err)
	require.Equal(t, feedID, resp.Note.NoteID)
	require.False(t, strings.TrimSpace(resp.Note.Title) == "" && strings.TrimSpace(resp.Note.Desc) == "", "title/desc 不应同时为空")
	t.Logf("ok: titleLen=%d descLen=%d images=%d", len(resp.Note.Title), len(resp.Note.Desc), len(resp.Note.ImageList))
}
