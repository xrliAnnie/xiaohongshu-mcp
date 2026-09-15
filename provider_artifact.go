package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"syscall"

	"golang.org/x/sys/unix"
)

var errProviderArtifact = errors.New("provider_artifact_invalid")

type providerArtifactStore struct {
	mu       sync.Mutex
	root     string
	info     os.FileInfo
	count    int
	bytes    int64
	poisoned bool
}
type providerArtifact struct {
	store      *providerArtifactStore
	name       string
	info       os.FileInfo
	descriptor frozenArtifact
}

// root is a new trusted lease scratch directory, never a model path or URL.
func newProviderArtifactStore(root string) (*providerArtifactStore, error) {
	if !filepath.IsAbs(root) || filepath.Clean(root) != root {
		return nil, errProviderArtifact
	}
	info, err := os.Lstat(root)
	if err != nil || !privateJournalEntry(info, true) {
		return nil, errProviderArtifact
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 0 {
		return nil, errProviderArtifact
	}
	return &providerArtifactStore{root: root, info: info}, nil
}
func (s *providerArtifactStore) directory() (*os.File, error) {
	fd, err := unix.Open(s.root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, errProviderArtifact
	}
	f := os.NewFile(uintptr(fd), "provider-media")
	info, err := f.Stat()
	if err != nil || !privateJournalEntry(info, true) || !os.SameFile(info, s.info) {
		f.Close()
		return nil, errProviderArtifact
	}
	return f, nil
}
func providerMediaFile(info os.FileInfo, size int64) bool {
	if info == nil {
		return false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == uint32(os.Geteuid()) && stat.Nlink == 1 && info.Mode().IsRegular() && info.Mode().Perm() == 0600 && info.Size() == size
}
func (s *providerArtifactStore) importArtifact(ctx context.Context, d frozenArtifact, input io.Reader, decode func(context.Context, string, string) error) (result *providerArtifact, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.poisoned || ctx.Err() != nil || input == nil || decode == nil || !frozenID(d.ArtifactID) || !journalDigest.MatchString(d.SHA256) || d.SizeBytes < 1 || d.SizeBytes > 10*1024*1024 || s.count >= 18 || s.bytes+d.SizeBytes > 180*1024*1024 {
		return nil, errProviderArtifact
	}
	switch d.MIMEType {
	case "image/png", "image/jpeg", "image/webp", "video/mp4":
	default:
		return nil, errProviderArtifact
	}
	dir, err := s.directory()
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	var nonce [32]byte
	if _, err = rand.Read(nonce[:]); err != nil {
		return nil, errProviderArtifact
	}
	extension := map[string]string{"image/png": ".png", "image/jpeg": ".jpg", "image/webp": ".webp", "video/mp4": ".mp4"}[d.MIMEType]
	name := hex.EncodeToString(nonce[:]) + extension
	file, err := createJournalFile(dir, name)
	if err != nil {
		return nil, errProviderArtifact
	}
	accepted := false
	defer func() {
		file.Close()
		if !accepted {
			if unix.Unlinkat(int(dir.Fd()), name, 0) != nil {
				s.poisoned = true
			}
		}
	}()
	// The private HTTP transport must also enforce a request deadline; a Reader
	// cannot interrupt an arbitrary blocked implementation on its own.
	source := &artifactContextReader{ctx: ctx, source: input}
	n, err := io.Copy(file, io.LimitReader(source, d.SizeBytes+1))
	if err != nil || n != d.SizeBytes || ctx.Err() != nil {
		return nil, errProviderArtifact
	}
	if file.Sync() != nil {
		return nil, errProviderArtifact
	}
	info, err := file.Stat()
	if err != nil || !providerMediaFile(info, d.SizeBytes) {
		return nil, errProviderArtifact
	}
	if file.Close() != nil || dir.Sync() != nil {
		return nil, errProviderArtifact
	}
	a := &providerArtifact{store: s, name: name, info: info, descriptor: d}
	if a.recheck() != nil {
		return nil, errProviderArtifact
	}
	if decode(ctx, a.path(), d.MIMEType) != nil || ctx.Err() != nil || a.recheck() != nil {
		return nil, errProviderArtifact
	}
	s.count++
	s.bytes += d.SizeBytes
	accepted = true
	return a, nil
}

type artifactContextReader struct {
	ctx    context.Context
	source io.Reader
}

func (r *artifactContextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.source.Read(p)
}
func (a *providerArtifact) path() string { return filepath.Join(a.store.root, a.name) }
func (a *providerArtifact) recheck() error {
	dir, err := a.store.directory()
	if err != nil {
		return err
	}
	defer dir.Close()
	fd, err := unix.Openat(int(dir.Fd()), a.name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return errProviderArtifact
	}
	f := os.NewFile(uintptr(fd), "verified-media")
	defer f.Close()
	before, err := f.Stat()
	if err != nil || !providerMediaFile(before, a.descriptor.SizeBytes) || !os.SameFile(before, a.info) {
		return errProviderArtifact
	}
	hash := sha256.New()
	header := make([]byte, 512)
	n, err := io.ReadFull(f, header)
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return errProviderArtifact
	}
	hash.Write(header[:n])
	rest, err := io.Copy(hash, io.LimitReader(f, a.descriptor.SizeBytes+1))
	if err != nil || int64(n)+rest != a.descriptor.SizeBytes || hex.EncodeToString(hash.Sum(nil)) != a.descriptor.SHA256 || http.DetectContentType(header[:n]) != a.descriptor.MIMEType {
		return errProviderArtifact
	}
	after, err := f.Stat()
	current, pathErr := os.Lstat(a.path())
	root, rootErr := os.Lstat(a.store.root)
	if err != nil || pathErr != nil || rootErr != nil || !providerMediaFile(after, a.descriptor.SizeBytes) || !providerMediaFile(current, a.descriptor.SizeBytes) || !os.SameFile(before, current) || !before.ModTime().Equal(after.ModTime()) || !privateJournalEntry(root, true) || !os.SameFile(root, a.store.info) {
		return errProviderArtifact
	}
	return nil
}
