package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
)

// This handler also requires the kernel-derived context so accidental mounting
// on the legacy TCP server cannot expose private operations.
func guardedRoutes(s *guardedService, uid uint32) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "application/json")
		reply := func(status int, value any) { w.WriteHeader(status); _ = json.NewEncoder(w).Encode(value) }
		deny := func(status int, code string) { reply(status, map[string]string{"code": code}) }
		defer func() {
			if recover() != nil {
				deny(500, "private_provider_unavailable")
			}
		}()
		peer, ok := r.Context().Value(providerPeerKey{}).(providerPeer)
		if !ok || !peer.valid || peer.uid != uid {
			deny(403, "private_peer_denied")
			return
		}
		if s == nil || r.Method != "POST" || r.URL.RawQuery != "" {
			deny(404, "private_route_denied")
			return
		}
		switch r.URL.Path {
		case "/v1/prepare":
			r.Body = http.MaxBytesReader(w, r.Body, 182*1024*1024)
			reader, err := r.MultipartReader()
			if err != nil {
				deny(400, "invalid_write_input")
				return
			}
			first, err := reader.NextPart()
			if err != nil || first.FormName() != "frozen" || first.FileName() != "" {
				deny(400, "invalid_write_input")
				return
			}
			raw, err := io.ReadAll(io.LimitReader(first, 1024*1024+1))
			first.Close()
			if err != nil || len(raw) > 1024*1024 {
				deny(400, "invalid_write_input")
				return
			}
			lease, err := s.prepare(r.Context(), raw, r.Header.Get("X-Content-Digest"), func(int) (io.Reader, error) {
				part, err := reader.NextPart()
				if err != nil {
					return nil, err
				}
				if part.FormName() != "media" || part.FileName() != "" {
					part.Close()
					return nil, errProviderArtifact
				}
				return part, nil
			})
			if err != nil {
				code := "invalid_write_input"
				status := 400
				if err == errAccountBusy {
					code = "account_busy"
					status = 409
				}
				deny(status, code)
				return
			}
			reply(200, lease)
		case "/v1/commit":
			var request struct {
				LeaseID   string          `json:"leaseId"`
				Permit    json.RawMessage `json:"permit"`
				Signature string          `json:"signature"`
			}
			if !decodePrivateRequest(w, r, &request, "leaseId", "permit", "signature") {
				deny(400, "invalid_write_input")
				return
			}
			reply(200, map[string]string{"state": s.commit(r.Context(), request.LeaseID, request.Permit, request.Signature)})
		case "/v1/status":
			var request struct {
				ReceiptID     string `json:"receiptId"`
				AttemptID     string `json:"attemptId"`
				ContentDigest string `json:"contentDigest"`
			}
			if !decodePrivateRequest(w, r, &request, "receiptId", "attemptId", "contentDigest") {
				deny(400, "invalid_write_input")
				return
			}
			reply(200, map[string]string{"state": s.status(request.ReceiptID, request.AttemptID, request.ContentDigest)})
		default:
			deny(404, "private_route_denied")
		}
	})
}
func decodePrivateRequest(w http.ResponseWriter, r *http.Request, value any, keys ...string) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 8192)
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		return false
	}
	unique := json.NewDecoder(bytes.NewReader(raw))
	object, err := uniqueRPCValue(unique, 0)
	if err != nil {
		return false
	}
	if _, err = unique.Token(); err != io.EOF {
		return false
	}
	fields, ok := object.(map[string]any)
	if !ok || len(fields) != len(keys) {
		return false
	}
	for _, key := range keys {
		if _, ok := fields[key]; !ok {
			return false
		}
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	return decoder.Decode(value) == nil
}
