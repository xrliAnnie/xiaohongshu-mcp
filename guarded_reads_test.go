package main

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"
)

var privateReadCases = []struct{ operation, body string }{
	{"search_feeds", `{"keyword":"fixture","filters":{},"limit":20}`},
	{"get_feed_detail", `{"feed_id":"feed-a","xsec_token":"synthetic-token","load_all_comments":false,"limit":20,"click_more_replies":false,"reply_limit":10,"scroll_speed":"normal"}`},
	{"user_profile", `{"user_id":"user-a","xsec_token":"synthetic-token"}`},
	{"list_collections", `{"limit":20}`},
	{"get_collection_content", `{"collection_id":"board-a","limit":20}`},
	{"list_saved_content", `{"limit":20}`},
}

type privateReadSessionFixture struct {
	*leaseSessionFixture
	reads     int
	operation string
}

func (s *privateReadSessionFixture) readPrivate(ctx context.Context, operation string, input any) (any, error) {
	s.reads++
	s.operation = operation
	return map[string]string{"operation": operation}, nil
}
func TestPrivateReadsUseSameAccountOwnerAndRejectUnclassifiedInput(t *testing.T) {
	for _, test := range privateReadCases {
		t.Run(test.operation, func(t *testing.T) {
			s, _, _, writes := guardedServiceFixture(t)
			account, _, _ := s.config.Epochs.current()
			owned := &privateReadSessionFixture{leaseSessionFixture: &leaseSessionFixture{id: account.AccountUserID}}
			s.open = func(context.Context, string, frozenAccount) (accountLeaseSession, error) { return owned, nil }
			_, client := privateProviderFixture(t, uint32(os.Geteuid()), guardedRoutes(s, uint32(os.Geteuid())))
			path := "http://private/v1/read/" + test.operation
			response, err := client.Post(path, "application/json", strings.NewReader(test.body))
			if err != nil {
				t.Fatal(err)
			}
			var result struct {
				Account  frozenAccount     `json:"account"`
				Upstream frozenUpstream    `json:"upstream"`
				Data     map[string]string `json:"data"`
			}
			decodeErr := json.NewDecoder(response.Body).Decode(&result)
			response.Body.Close()
			if response.StatusCode != http.StatusOK || decodeErr != nil || result.Account != account || result.Upstream != s.config.Upstream || result.Data["operation"] != test.operation {
				t.Fatal("private read missing or unbound", response.StatusCode, decodeErr)
			}
			if owned.reads != 1 || owned.operation != test.operation || owned.closes.Load() != 1 || writes.Load() != 0 {
				t.Fatal("incorrect read ownership or dispatched write")
			}
			var invalid map[string]any
			json.Unmarshal([]byte(test.body), &invalid)
			invalid["approved"] = true
			raw, _ := json.Marshal(invalid)
			response, err = client.Post(path, "application/json", strings.NewReader(string(raw)))
			if err != nil {
				t.Fatal(err)
			}
			response.Body.Close()
			if response.StatusCode != 400 || owned.reads != 1 {
				t.Fatal("accepted extra input")
			}
		})
	}
}
func TestPrivateReadsCannotTakePreparedWriteLease(t *testing.T) {
	for _, test := range privateReadCases {
		t.Run(test.operation, func(t *testing.T) {
			s, raw, digest, writes := guardedServiceFixture(t)
			if _, err := s.prepare(context.Background(), raw, digest, nil); err != nil {
				t.Fatal(err)
			}
			_, client := privateProviderFixture(t, uint32(os.Geteuid()), guardedRoutes(s, uint32(os.Geteuid())))
			response, err := client.Post("http://private/v1/read/"+test.operation, "application/json", strings.NewReader(test.body))
			if err != nil {
				t.Fatal(err)
			}
			response.Body.Close()
			if response.StatusCode != 409 || writes.Load() != 0 {
				t.Fatal("read stole prepared account", response.StatusCode)
			}
		})
	}
}

func TestPrivateReadsRejectMalformedInputsBeforeBrowser(t *testing.T) {
	s, _, _, _ := guardedServiceFixture(t)
	s.open = func(context.Context, string, frozenAccount) (accountLeaseSession, error) {
		t.Error("invalid read opened browser")
		return nil, errPrivateProvider
	}
	_, client := privateProviderFixture(t, uint32(os.Geteuid()), guardedRoutes(s, uint32(os.Geteuid())))
	for _, test := range []struct{ operation, body string }{
		{"search_feeds", `{"keyword":"fixture","filters":{"sort_by":"综合|最新"},"limit":20}`},
		{"search_feeds", `{"keyword":"fixture","filters":{"unknown":"x"},"limit":20}`},
		{"search_feeds", `{"keyword":"fixture","filters":{"sort_by":null},"limit":20}`},
		{"search_feeds", `{"keyword":"fixture","filters":{},"limit":201}`},
		{"search_feeds", `{"keyword":"fixture","filters":{},"limit":null}`},
		{"search_feeds", `{"keyword":"fixture","filters":{},"limit":20,"limit":21}`},
		{"search_feeds", "{\"keyword\":\"\xff\",\"filters\":{},\"limit\":20}"},
		{"get_feed_detail", `{"feed_id":"../other","xsec_token":"synthetic-token","load_all_comments":false,"limit":20,"click_more_replies":false,"reply_limit":10,"scroll_speed":"normal"}`},
		{"user_profile", `{"user_id":"user-a","xsec_token":"bad\nsynthetic-token"}`},
		{"list_collections", `{"limit":501}`},
		{"get_collection_content", `{"collection_id":"board-a","limit":0}`},
		{"list_saved_content", `{"limit":-1}`},
	} {
		response, err := client.Post("http://private/v1/read/"+test.operation, "application/json", strings.NewReader(test.body))
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != 400 {
			t.Errorf("%s invalid read status %d", test.operation, response.StatusCode)
		}
	}
}
