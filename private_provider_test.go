package main

import (
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

func privateProviderFixture(t *testing.T, uid uint32, handler http.Handler) (*privateProviderHTTP, *http.Client) {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "xhs-peer-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	server, err := newPrivateProviderHTTP(filepath.Join(dir, "provider.sock"), uid, handler)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- server.Serve() }()
	t.Cleanup(func() {
		server.Close()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("server did not stop")
		}
	})
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", filepath.Join(dir, "provider.sock"))
	}}
	t.Cleanup(transport.CloseIdleConnections)
	return server, &http.Client{Transport: transport, Timeout: 2 * time.Second}
}
func TestGuardedWritePrivateSocketUsesKernelUID(t *testing.T) {
	var calls atomic.Int32
	server, client := privateProviderFixture(t, uint32(os.Geteuid()), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(204) }))
	req, _ := http.NewRequest("GET", "http://private/status", nil)
	req.Header.Set("X-Peer-Uid", "0")
	response, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 204 || calls.Load() != 1 {
		t.Fatal("real peer was not admitted")
	}
	info, err := os.Lstat(server.path)
	if err != nil || info.Mode().Perm() != 0600 || info.Mode()&os.ModeSocket == 0 {
		t.Fatal("socket is not private")
	}
	if server.listener.Addr().Network() != "unix" {
		t.Fatal("listener is not unix")
	}
}
func TestGuardedWritePrivateSocketRejectsClaimedUID(t *testing.T) {
	var calls atomic.Int32
	_, client := privateProviderFixture(t, uint32(os.Geteuid()+1), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(204) }))
	req, _ := http.NewRequest("GET", "http://private/status", nil)
	req.Header.Set("X-Peer-Uid", strconv.Itoa(os.Geteuid()+1))
	response, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 403 || calls.Load() != 0 {
		t.Fatalf("status=%d calls=%d", response.StatusCode, calls.Load())
	}
}
func TestGuardedWritePrivateSocketRefusesUnsafeOrExistingPaths(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "xhs-peer-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "provider.sock")
	if err = os.WriteFile(path, []byte("existing"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = newPrivateProviderHTTP(path, uint32(os.Geteuid()), http.NotFoundHandler()); err == nil {
		t.Fatal("replaced existing path")
	}
	data, _ := os.ReadFile(path)
	if string(data) != "existing" {
		t.Fatal("modified existing path")
	}
	os.Remove(path)
	os.Chmod(dir, 0755)
	if _, err = newPrivateProviderHTTP(path, uint32(os.Geteuid()), http.NotFoundHandler()); err == nil {
		t.Fatal("accepted shared parent")
	}
	os.Chmod(dir, 0700)
	alias := dir + "-link"
	if err = os.Symlink(dir, alias); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(alias)
	if _, err = newPrivateProviderHTTP(filepath.Join(alias, "provider.sock"), uint32(os.Geteuid()), http.NotFoundHandler()); err == nil {
		t.Fatal("accepted symlink parent")
	}
}

func TestGuardedWritePrivateSocketCleanupPreservesReplacement(t *testing.T) {
	server, _ := privateProviderFixture(t, uint32(os.Geteuid()), http.NotFoundHandler())
	if err := os.Remove(server.path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(server.path, []byte("replacement"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := server.Close(); err == nil {
		t.Fatal("did not detect replacement")
	}
	data, err := os.ReadFile(server.path)
	if err != nil || string(data) != "replacement" {
		t.Fatal("removed replacement file")
	}
}
