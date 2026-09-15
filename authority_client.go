package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"time"
)

type authorityClient struct {
	path           string
	uid            uint32
	parent, socket os.FileInfo
	transport      *http.Transport
	client         *http.Client
}

// The path and UID come from trusted startup policy. Every request uses a new
// Unix connection with kernel peer checks; there is no proxy, TCP or redirect lane.
func newAuthorityClient(path string, uid uint32) (*authorityClient, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, errPrivateProvider
	}
	parent, err := os.Lstat(filepath.Dir(path))
	if err != nil {
		return nil, errPrivateProvider
	}
	socket, err := os.Lstat(path)
	if err != nil {
		return nil, errPrivateProvider
	}
	c := &authorityClient{path: path, uid: uid, parent: parent, socket: socket}
	if c.check() != nil {
		return nil, errPrivateProvider
	}
	c.transport = &http.Transport{DisableKeepAlives: true, MaxResponseHeaderBytes: 8192, ResponseHeaderTimeout: 5 * time.Second, DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
		if network != "tcp" || address != "authority:80" || c.check() != nil {
			return nil, errPrivateProvider
		}
		conn, err := (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "unix", c.path)
		if err != nil {
			return nil, errPrivateProvider
		}
		peer, err := unixPeerUID(conn)
		if err != nil || peer != c.uid || c.check() != nil {
			conn.Close()
			return nil, errPrivateProvider
		}
		return conn, nil
	}}
	c.client = &http.Client{Transport: c.transport, Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return c, nil
}
func (c *authorityClient) check() error {
	parent, err := os.Lstat(filepath.Dir(c.path))
	if err != nil || !os.SameFile(parent, c.parent) || !parent.IsDir() || parent.Mode().Perm() != 0700 {
		return errPrivateProvider
	}
	socket, err := os.Lstat(c.path)
	if err != nil || !os.SameFile(socket, c.socket) || socket.Mode()&os.ModeSocket == 0 || socket.Mode().Perm() != 0600 {
		return errPrivateProvider
	}
	for _, info := range []os.FileInfo{parent, socket} {
		owner, ok := info.Sys().(*syscall.Stat_t)
		if !ok || owner.Uid != c.uid {
			return errPrivateProvider
		}
	}
	return nil
}
func (c *authorityClient) post(ctx context.Context, path string, body []byte, response any) error {
	if ctx.Err() != nil || len(body) > 8192 {
		return errPrivateProvider
	}
	req, err := http.NewRequestWithContext(ctx, "POST", "http://authority"+path, bytes.NewReader(body))
	if err != nil {
		return errPrivateProvider
	}
	req.Header.Set("Content-Type", "application/json")
	result, err := c.client.Do(req)
	if err != nil {
		return errPrivateProvider
	}
	defer result.Body.Close()
	if result.StatusCode != 200 {
		return errPrivateProvider
	}
	raw, err := io.ReadAll(io.LimitReader(result.Body, 16385))
	if err != nil || len(raw) > 16384 || !strictAuthorityJSON(raw, response) {
		return errPrivateProvider
	}
	return nil
}
func strictAuthorityJSON(raw []byte, value any) bool {
	parse := func(data []byte) (any, error) {
		d := json.NewDecoder(bytes.NewReader(data))
		d.UseNumber()
		v, err := uniqueRPCValue(d, 0)
		if err != nil {
			return nil, err
		}
		if _, err = d.Token(); err != io.EOF {
			return nil, errPrivateProvider
		}
		return v, nil
	}
	incoming, err := parse(raw)
	if err != nil {
		return false
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(value) != nil {
		return false
	}
	normalized, err := json.Marshal(value)
	if err != nil {
		return false
	}
	expected, err := parse(normalized)
	return err == nil && reflect.DeepEqual(incoming, expected)
}
func (c *authorityClient) admit(ctx context.Context, permit internalPermit) error {
	raw, err := canonicalInternalPermit(permit)
	if err != nil {
		return errPrivateProvider
	}
	var response struct {
		Admitted     bool   `json:"admitted"`
		PermitDigest string `json:"permitDigest"`
	}
	if c.post(ctx, "/internal/v1/provider-admission", raw, &response) != nil {
		return errPrivateProvider
	}
	digest := sha256.Sum256(raw)
	if !response.Admitted || response.PermitDigest != hex.EncodeToString(digest[:]) {
		return errPrivateProvider
	}
	return nil
}
func (c *authorityClient) resolve(ctx context.Context, permit internalPermit, account frozenAccount, target frozenTarget) (string, error) {
	permitRaw, err := canonicalInternalPermit(permit)
	if err != nil {
		return "", errPrivateProvider
	}
	permitDigest := sha256.Sum256(permitRaw)
	request := struct {
		Permit  internalPermit `json:"permit"`
		Account frozenAccount  `json:"account"`
		Target  frozenTarget   `json:"target"`
	}{permit, account, target}
	raw, err := json.Marshal(request)
	if err != nil {
		return "", errPrivateProvider
	}
	var response struct {
		PermitDigest string        `json:"permitDigest"`
		Account      frozenAccount `json:"account"`
		Target       frozenTarget  `json:"target"`
		Token        string        `json:"token"`
	}
	if c.post(ctx, "/internal/v1/provider-token", raw, &response) != nil || response.PermitDigest != hex.EncodeToString(permitDigest[:]) || response.Account != account || !reflect.DeepEqual(response.Target, target) || response.Token == "" || len(response.Token) > 4096 || strings.ContainsAny(response.Token, "\x00\r\n") {
		return "", errPrivateProvider
	}
	return response.Token, nil
}
func (c *authorityClient) Close() { c.transport.CloseIdleConnections() }
