package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

var publicReadTools = map[string]bool{
	"check_login_status": true, "get_login_qrcode": true, "delete_cookies": true,
	"list_feeds": true, "search_feeds": true, "get_feed_detail": true, "user_profile": true,
	"list_collections": true, "get_collection_content": true, "list_saved_content": true,
}
var publicREST = map[string]bool{
	"GET /api/v1/login/status": true, "GET /api/v1/login/qrcode": true, "DELETE /api/v1/login/cookies": true,
	"GET /api/v1/feeds/list": true, "GET /api/v1/feeds/search": true, "POST /api/v1/feeds/search": true,
	"POST /api/v1/feeds/detail": true, "POST /api/v1/user/profile": true, "GET /api/v1/user/me": true,
}

// The public listener never carries a write permit. Unknown tool/path aliases fail closed too.
func publicWriteGuard() gin.HandlerFunc {
	return func(c *gin.Context) {
		deny := func() { c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"code": "founder_write_gate_absent"}) }
		// OPTIONS performs no operation; preserve existing read CORS negotiation.
		if c.Request.Method == http.MethodOptions {
			c.Next()
			return
		}
		path := c.Request.URL.Path
		if strings.HasPrefix(path, "/api/v1/") && !publicREST[c.Request.Method+" "+path] {
			deny()
			return
		}
		if (path == "/mcp" || strings.HasPrefix(path, "/mcp/")) && c.Request.Method == http.MethodPost {
			data, err := io.ReadAll(http.MaxBytesReader(c.Writer, c.Request.Body, 1024*1024))
			_ = c.Request.Body.Close()
			if err != nil {
				deny()
				return
			}
			c.Request.Body = io.NopCloser(bytes.NewReader(data))
			decoder := json.NewDecoder(bytes.NewReader(data))
			decoder.UseNumber()
			value, err := uniqueRPCValue(decoder, 0)
			if err != nil {
				deny()
				return
			}
			if _, err = decoder.Token(); err != io.EOF || !publicRPCAllowed(value) {
				deny()
				return
			}
		}
		c.Next()
	}
}
func publicRPCAllowed(value any) bool {
	if batch, ok := value.([]any); ok {
		if len(batch) == 0 || len(batch) > 64 {
			return false
		}
		for _, item := range batch {
			if _, ok := item.(map[string]any); !ok || !publicRPCAllowed(item) {
				return false
			}
		}
		return true
	}
	object, ok := value.(map[string]any)
	if !ok || rpcCaseAlias(object, "method", "params") {
		return false
	}
	method, ok := object["method"].(string)
	if !ok {
		return false
	}
	if method != "tools/call" {
		return true
	}
	params, ok := object["params"].(map[string]any)
	if !ok || rpcCaseAlias(params, "name") {
		return false
	}
	name, ok := params["name"].(string)
	return ok && publicReadTools[name]
}
func rpcCaseAlias(object map[string]any, names ...string) bool {
	for key := range object {
		for _, name := range names {
			if key != name && strings.EqualFold(key, name) {
				return true
			}
		}
	}
	return false
}

// Reject ambiguous duplicate keys, including nested parameters, before either JSON parser sees them.
func uniqueRPCValue(decoder *json.Decoder, depth int) (any, error) {
	if depth > 32 {
		return nil, errors.New("rpc_depth")
	}
	token, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return token, nil
	}
	switch delimiter {
	case '{':
		object := map[string]any{}
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return nil, err
			}
			key, ok := keyToken.(string)
			if !ok {
				return nil, errors.New("rpc_key")
			}
			if _, exists := object[key]; exists {
				return nil, errors.New("rpc_duplicate")
			}
			value, err := uniqueRPCValue(decoder, depth+1)
			if err != nil {
				return nil, err
			}
			object[key] = value
		}
		close, err := decoder.Token()
		if err != nil || close != json.Delim('}') {
			return nil, errors.New("rpc_object")
		}
		return object, nil
	case '[':
		values := []any{}
		for decoder.More() {
			value, err := uniqueRPCValue(decoder, depth+1)
			if err != nil {
				return nil, err
			}
			values = append(values, value)
		}
		close, err := decoder.Token()
		if err != nil || close != json.Delim(']') {
			return nil, errors.New("rpc_array")
		}
		return values, nil
	default:
		return nil, errors.New("rpc_delimiter")
	}
}
