package main

import (
	"context"
	"encoding/json"
	"github.com/go-rod/rod/lib/proto"
	"github.com/xpzouying/xiaohongshu-mcp/browser"
	"os"
	"path/filepath"
	"testing"
)

func sessionCookieFixture(t *testing.T) (string, frozenAccount) {
	t.Helper()
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	account := frozenAccount{ProviderInstanceID: "provider-a", AccountUserID: "account-a", AccountEpoch: 1, ProviderGeneration: "generation-a"}
	record := controlledCookieRecord{SchemaVersion: 1, Account: account, Cookies: []*proto.NetworkCookieParam{{Name: "synthetic", Value: "test-only", Domain: ".xiaohongshu.com", Path: "/"}}}
	raw, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "account.json")
	if err = os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	return path, account
}
func TestControlledCookiesArePrivateAndBound(t *testing.T) {
	path, account := sessionCookieFixture(t)
	record, err := loadControlledCookies(path, account)
	if err != nil || len(record.Cookies) != 1 {
		t.Fatal("valid snapshot", err)
	}
	wrong := account
	wrong.AccountEpoch++
	if _, err = loadControlledCookies(path, wrong); err == nil {
		t.Fatal("stale epoch accepted")
	}
	if err = os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err = loadControlledCookies(path, account); err == nil {
		t.Fatal("public cookies accepted")
	}
}
func TestControlledCookiesRejectLinksAndMalformed(t *testing.T) {
	for _, kind := range []string{"symlink", "hardlink", "empty", "duplicate", "unknown", "foreign"} {
		t.Run(kind, func(t *testing.T) {
			path, account := sessionCookieFixture(t)
			switch kind {
			case "symlink":
				if err := os.Rename(path, path+".original"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(path+".original", path); err != nil {
					t.Fatal(err)
				}
			case "hardlink":
				if err := os.Link(path, path+".copy"); err != nil {
					t.Fatal(err)
				}
			case "empty":
				if err := os.WriteFile(path, []byte(`{}`), 0600); err != nil {
					t.Fatal(err)
				}
			default:
				raw, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if kind == "duplicate" {
					raw = append([]byte(`{"schemaVersion":1,`), raw[1:]...)
				}
				if kind == "unknown" {
					raw = append([]byte(`{"unexpected":true,`), raw[1:]...)
				}
				if kind == "foreign" {
					var record controlledCookieRecord
					if json.Unmarshal(raw, &record) != nil {
						t.Fatal("decode")
					}
					record.Cookies[0].Domain = "evil.example"
					raw, _ = json.Marshal(record)
				}
				if err = os.WriteFile(path, raw, 0600); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := loadControlledCookies(path, account); err == nil {
				t.Fatal("invalid snapshot accepted")
			}
		})
	}
}
func TestControlledSessionFailsBeforeBrowserForBadSnapshot(t *testing.T) {
	path, account := sessionCookieFixture(t)
	account.AccountEpoch++
	// No executable is configured: a stale snapshot must fail at cookie admission.
	session, err := openControlledSession(context.Background(), browser.PipeBrowserOptions{}, path, account)
	if session != nil || err != errControlledCookies {
		t.Fatal("did not reject cookie binding before launch", err)
	}
}
