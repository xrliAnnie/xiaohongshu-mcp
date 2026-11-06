package main

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
)

func TestGuardedRoutesUsePrivateSocketForPrepareCommitStatus(t *testing.T) {
	s, raw, digest, calls := guardedServiceFixture(t)
	_, client := privateProviderFixture(t, uint32(os.Geteuid()), guardedRoutes(s, uint32(os.Geteuid())))
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormField("frozen")
	if err != nil {
		t.Fatal(err)
	}
	part.Write(raw)
	writer.Close()
	req, _ := http.NewRequest("POST", "http://private/v1/prepare", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("X-Content-Digest", digest)
	response, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var lease preparedLease
	if json.NewDecoder(response.Body).Decode(&lease) != nil {
		t.Fatal("response")
	}
	response.Body.Close()
	if response.StatusCode != 200 || lease.LeaseID == "" || calls.Load() != 0 {
		t.Fatal("bad prepare", response.StatusCode)
	}
	permit, sig := servicePermit(t, s, lease, raw)
	wire, _ := json.Marshal(map[string]any{"leaseId": lease.LeaseID, "permit": json.RawMessage(permit), "signature": sig})
	for i := 0; i < 2; i++ {
		response, err = client.Post("http://private/v1/commit", "application/json", bytes.NewReader(wire))
		if err != nil {
			t.Fatal(err)
		}
		var result map[string]string
		if json.NewDecoder(response.Body).Decode(&result) != nil {
			t.Fatal("result")
		}
		response.Body.Close()
		if result["state"] != "succeeded" {
			t.Fatal("commit did not succeed", result)
		}
	}
	if calls.Load() != 1 {
		t.Fatal("replayed")
	}
	status, _ := json.Marshal(map[string]string{"receiptId": "receipt-a", "attemptId": "attempt-a", "contentDigest": digest})
	response, err = client.Post("http://private/v1/status", "application/json", bytes.NewReader(status))
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if !bytes.Contains(data, []byte(`"succeeded"`)) {
		t.Fatal("status")
	}
}
func TestGuardedRoutesDenyClaimedPeerAndExtraMedia(t *testing.T) {
	s, raw, digest, calls := guardedServiceFixture(t)
	direct := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/status", bytes.NewBufferString(`{}`))
	req.Header.Set("X-Peer-Uid", "0")
	guardedRoutes(s, uint32(os.Geteuid())).ServeHTTP(direct, req)
	if direct.Code != 403 {
		t.Fatal("header supplied peer")
	}
	_, client := privateProviderFixture(t, uint32(os.Geteuid()), guardedRoutes(s, uint32(os.Geteuid())))
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, _ := writer.CreateFormField("frozen")
	part.Write(raw)
	part, _ = writer.CreateFormField("media")
	part.Write([]byte("unexpected"))
	writer.Close()
	req, _ = http.NewRequest("POST", "http://private/v1/prepare", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("X-Content-Digest", digest)
	response, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 400 || calls.Load() != 0 {
		t.Fatal("extra media accepted")
	}
	s.manager.mu.Lock()
	active := s.manager.active
	s.manager.mu.Unlock()
	if active != nil {
		t.Fatal("bad media opened browser lease")
	}
}

func TestGuardedRoutesRejectAmbiguousRequestKeys(t *testing.T) {
	s, _, _, _ := guardedServiceFixture(t)
	_, client := privateProviderFixture(t, uint32(os.Geteuid()), guardedRoutes(s, uint32(os.Geteuid())))
	for _, wire := range []string{
		`{"receiptId":"a","ReceiptId":"b","attemptId":"a","contentDigest":"bad"}`,
		`{"ReceiptId":"a","attemptId":"a","contentDigest":"bad"}`,
		`{"receiptId":"a","attemptId":"a","contentDigest":"bad","unexpected":true}`,
		`{"receiptId":"a","receiptId":"b","attemptId":"a","contentDigest":"bad"}`,
	} {
		response, err := client.Post("http://private/v1/status", "application/json", bytes.NewBufferString(wire))
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != 400 {
			t.Fatal("ambiguous keys accepted", response.StatusCode)
		}
	}
}
