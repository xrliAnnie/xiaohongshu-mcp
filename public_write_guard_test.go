package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func guardedTestRouter(next *int) *gin.Engine {
	gin.SetMode(gin.ReleaseMode)
	router := gin.New()
	router.Use(publicWriteGuard())
	router.Any("/*path", func(c *gin.Context) { *next++; c.Status(204) })
	return router
}
func TestGuardedWritePublicMCPAlwaysDenies(t *testing.T) {
	for _, name := range []string{"publish_content", "publish_with_video", "post_comment_to_feed", "reply_comment_in_feed", "like_feed", "favorite_feed", "future_write_alias"} {
		for _, path := range []string{"/mcp", "/mcp/anything"} {
			t.Run(name+path, func(t *testing.T) {
				count := 0
				router := guardedTestRouter(&count)
				body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"` + name + `","arguments":{}}}`
				result := httptest.NewRecorder()
				router.ServeHTTP(result, httptest.NewRequest("POST", path, strings.NewReader(body)))
				if result.Code != http.StatusForbidden || count != 0 || !strings.Contains(result.Body.String(), "founder_write_gate_absent") {
					t.Fatalf("status=%d downstream=%d body=%s", result.Code, count, result.Body.String())
				}
			})
		}
	}
}
func TestGuardedWritePublicRESTDeniesBeforeParsing(t *testing.T) {
	for _, path := range []string{"/api/v1/publish", "/api/v1/publish_video", "/api/v1/feeds/comment", "/api/v1/feeds/comment/reply", "/api/v1/feeds/like", "/api/v1/feeds/favorite", "/api/v1/future-write"} {
		count := 0
		router := guardedTestRouter(&count)
		result := httptest.NewRecorder()
		router.ServeHTTP(result, httptest.NewRequest("POST", path, strings.NewReader("not even JSON")))
		if result.Code != 403 || count != 0 {
			t.Fatalf("%s status=%d downstream=%d", path, result.Code, count)
		}
	}
}
func TestGuardedWriteRejectsBatchAndCaseAliases(t *testing.T) {
	for _, body := range []string{
		`[{"method":"tools/list"},{"method":"tools/call","params":{"name":"like_feed"}}]`,
		`{"Method":"tools/call","Params":{"Name":"like_feed"}}`,
		`{"method":"tools/call","params":{"name":"list_feeds","Name":"like_feed"}}`,
		`{"method":"tools/call","params":{"name":"list_feeds"},"params":{"name":"like_feed"}}`,
	} {
		count := 0
		router := guardedTestRouter(&count)
		result := httptest.NewRecorder()
		router.ServeHTTP(result, httptest.NewRequest("POST", "/mcp", strings.NewReader(body)))
		if result.Code != 403 || count != 0 {
			t.Fatalf("status=%d downstream=%d", result.Code, count)
		}
	}
}
func TestGuardedWritePreservesReadBody(t *testing.T) {
	gin.SetMode(gin.ReleaseMode)
	router := gin.New()
	router.Use(publicWriteGuard())
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_feed_detail","arguments":{"feed_id":"synthetic"}}}`
	router.POST("/mcp", func(c *gin.Context) {
		data, err := io.ReadAll(c.Request.Body)
		if err != nil || string(data) != body {
			t.Fatal("read body changed")
		}
		c.Status(204)
	})
	result := httptest.NewRecorder()
	router.ServeHTTP(result, httptest.NewRequest("POST", "/mcp", strings.NewReader(body)))
	if result.Code != 204 {
		t.Fatalf("read blocked: %d", result.Code)
	}
}
func TestGuardedWriteIsInstalledOnActualPublicRouter(t *testing.T) {
	router := setupRoutes(&AppServer{})
	result := httptest.NewRecorder()
	router.ServeHTTP(result, httptest.NewRequest("POST", "/mcp/alias", strings.NewReader(`{"method":"tools/call","params":{"name":"publish_content"}}`)))
	if result.Code != 403 {
		t.Fatalf("public route status=%d", result.Code)
	}
}

func TestGuardedWritePreservesReadCORSPreflight(t *testing.T) {
	count := 0
	router := guardedTestRouter(&count)
	result := httptest.NewRecorder()
	router.ServeHTTP(result, httptest.NewRequest("OPTIONS", "/api/v1/feeds/search", nil))
	if result.Code != 204 || count != 1 {
		t.Fatalf("preflight status=%d downstream=%d", result.Code, count)
	}
}
