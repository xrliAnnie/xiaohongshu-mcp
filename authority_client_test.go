package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"sync/atomic"
	"testing"
)

func TestAuthorityClientBindsAdmissionAndTokenOverPrivateSocket(t *testing.T) {
	_, raw, _, _ := executionFixture(t)
	var permit internalPermit
	if json.Unmarshal(raw, &permit) != nil {
		t.Fatal("permit")
	}
	account := frozenAccount{ProviderInstanceID: "provider-a", AccountUserID: "account-a", AccountEpoch: 1, ProviderGeneration: "generation-a"}
	target := frozenTarget{FeedID: "feed-a"}
	server, _ := privateProviderFixture(t, uint32(os.Geteuid()), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/internal/v1/provider-admission":
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Error(err)
			}
			if string(body) != string(raw) {
				t.Error("permit bytes changed")
			}
			sum := sha256.Sum256(body)
			json.NewEncoder(w).Encode(map[string]any{"admitted": true, "permitDigest": hex.EncodeToString(sum[:])})
		case "/internal/v1/provider-token":
			sum := sha256.Sum256(raw)
			json.NewEncoder(w).Encode(map[string]any{"permitDigest": hex.EncodeToString(sum[:]), "account": account, "target": target, "token": "synthetic-token"})
		default:
			w.WriteHeader(404)
		}
	}))
	client, err := newAuthorityClient(server.path, uint32(os.Geteuid()))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if err = client.admit(context.Background(), permit); err != nil {
		t.Fatal(err)
	}
	if token, err := client.resolve(context.Background(), permit, account, target); err != nil || token != "synthetic-token" {
		t.Fatal("token binding", err)
	}
	changedPermit := permit
	changedPermit.AttemptID = "other-attempt"
	if _, err := client.resolve(context.Background(), changedPermit, account, target); err == nil {
		t.Fatal("token escaped attempt binding")
	}
	wrong := account
	wrong.AccountEpoch++
	if _, err = client.resolve(context.Background(), permit, wrong, target); err == nil {
		t.Fatal("token escaped account epoch")
	}
}
func TestAuthorityClientDeniesMismatchRedirectAndMalformed(t *testing.T) {
	_, raw, _, _ := executionFixture(t)
	var p internalPermit
	if json.Unmarshal(raw, &p) != nil {
		t.Fatal("permit")
	}
	for _, mode := range []string{"digest", "denied", "redirect", "unknown", "oversized"} {
		t.Run(mode, func(t *testing.T) {
			server, _ := privateProviderFixture(t, uint32(os.Geteuid()), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch mode {
				case "redirect":
					w.Header().Set("Location", "http://127.0.0.1:1/private")
					w.WriteHeader(307)
				case "unknown":
					io.WriteString(w, `{"admitted":true,"permitDigest":"wrong","extra":true}`)
				case "oversized":
					w.Write(make([]byte, 65537))
				default:
					json.NewEncoder(w).Encode(map[string]any{"admitted": mode != "denied", "permitDigest": "wrong"})
				}
			}))
			c, err := newAuthorityClient(server.path, uint32(os.Geteuid()))
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			if c.admit(context.Background(), p) == nil {
				t.Fatal("bad admission accepted")
			}
		})
	}
}
func TestAuthorityClientRejectsWrongOwner(t *testing.T) {
	server, _ := privateProviderFixture(t, uint32(os.Geteuid()), http.NotFoundHandler())
	if _, err := newAuthorityClient(server.path, uint32(os.Geteuid()+1)); err == nil {
		t.Fatal("accepted wrong authority UID")
	}
}

func TestAuthorityClientDoesNotRetryLostAdmissionResponse(t *testing.T) {
	_, raw, _, _ := executionFixture(t)
	var p internalPermit
	if json.Unmarshal(raw, &p) != nil {
		t.Fatal("permit")
	}
	var calls atomic.Int32
	server, _ := privateProviderFixture(t, uint32(os.Geteuid()), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		conn.Close()
	}))
	c, err := newAuthorityClient(server.path, uint32(os.Geteuid()))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err = c.admit(context.Background(), p); err != errPrivateProvider || calls.Load() != 1 {
		t.Fatal("response loss retried or leaked raw error", calls.Load(), err)
	}
}
func TestAuthorityClientRechecksSocketPermissionsBeforeEachRequest(t *testing.T) {
	_, raw, _, _ := executionFixture(t)
	var p internalPermit
	if json.Unmarshal(raw, &p) != nil {
		t.Fatal("permit")
	}
	var calls atomic.Int32
	server, _ := privateProviderFixture(t, uint32(os.Geteuid()), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(500) }))
	c, err := newAuthorityClient(server.path, uint32(os.Geteuid()))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if os.Chmod(server.path, 0644) != nil {
		t.Fatal("chmod")
	}
	defer os.Chmod(server.path, 0600)
	if c.admit(context.Background(), p) == nil || calls.Load() != 0 {
		t.Fatal("drifted socket used")
	}
}
