package main

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"
)

var errPrivateProvider = errors.New("private_provider_unavailable")

type providerPeerKey struct{}
type providerPeer struct {
	uid   uint32
	valid bool
}
type privateProviderHTTP struct {
	server         *http.Server
	listener       *net.UnixListener
	path           string
	parent, socket os.FileInfo
	once           sync.Once
	closeErr       error
}

// The caller supplies policy-owned paths and the pinned authority UID. No TCP alternative exists.
func newPrivateProviderHTTP(path string, expectedUID uint32, handler http.Handler) (*privateProviderHTTP, error) {
	if !filepath.IsAbs(path) || handler == nil {
		return nil, errPrivateProvider
	}
	parent, err := os.Lstat(filepath.Dir(path))
	if err != nil || !privateJournalEntry(parent, true) {
		return nil, errPrivateProvider
	}
	if _, err = os.Lstat(path); !os.IsNotExist(err) {
		return nil, errPrivateProvider
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, errPrivateProvider
	}
	listener.SetUnlinkOnClose(false)
	socket, err := os.Lstat(path)
	if err != nil || socket.Mode()&os.ModeSocket == 0 {
		listener.Close()
		return nil, errPrivateProvider
	}
	s := &privateProviderHTTP{listener: listener, path: path, parent: parent, socket: socket}
	if err = os.Chmod(path, 0600); err != nil {
		s.Close()
		return nil, errPrivateProvider
	}
	current, err := os.Lstat(filepath.Dir(path))
	if err != nil || !os.SameFile(parent, current) || !privateJournalEntry(current, true) {
		s.Close()
		return nil, errPrivateProvider
	}
	s.server = &http.Server{
		ReadHeaderTimeout: 2 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 2 * time.Minute, IdleTimeout: 5 * time.Second, MaxHeaderBytes: 8192,
		ConnContext: func(ctx context.Context, conn net.Conn) context.Context {
			uid, err := unixPeerUID(conn)
			return context.WithValue(ctx, providerPeerKey{}, providerPeer{uid: uid, valid: err == nil})
		},
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			peer, ok := r.Context().Value(providerPeerKey{}).(providerPeer)
			if !ok || !peer.valid || peer.uid != expectedUID {
				http.Error(w, "private_peer_denied", http.StatusForbidden)
				return
			}
			handler.ServeHTTP(w, r)
		}),
	}
	return s, nil
}
func unixPeerUID(conn net.Conn) (uint32, error) {
	socket, ok := conn.(*net.UnixConn)
	if !ok {
		return 0, errPrivateProvider
	}
	raw, err := socket.SyscallConn()
	if err != nil {
		return 0, errPrivateProvider
	}
	var uid uint32
	var credentialErr error
	if err = raw.Control(func(fd uintptr) { uid, credentialErr = kernelPeerUID(int(fd)) }); err != nil || credentialErr != nil {
		return 0, errPrivateProvider
	}
	return uid, nil
}
func (s *privateProviderHTTP) Serve() error { return s.server.Serve(s.listener) }
func (s *privateProviderHTTP) Close() error {
	s.once.Do(func() {
		if s.server != nil {
			s.server.Close()
		}
		s.listener.Close()
		parent, parentErr := os.Lstat(filepath.Dir(s.path))
		socket, socketErr := os.Lstat(s.path)
		if os.IsNotExist(socketErr) {
			return
		}
		if parentErr != nil || socketErr != nil || !os.SameFile(s.parent, parent) || !os.SameFile(s.socket, socket) {
			s.closeErr = errPrivateProvider
			return
		}
		s.closeErr = os.Remove(s.path)
	})
	return s.closeErr
}
