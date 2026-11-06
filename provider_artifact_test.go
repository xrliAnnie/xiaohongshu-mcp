package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func artifactFixture(t *testing.T) (*providerArtifactStore, frozenArtifact, []byte) {
	t.Helper()
	root := t.TempDir()
	if os.Chmod(root, 0700) != nil {
		t.Fatal("chmod")
	}
	s, err := newProviderArtifactStore(root)
	if err != nil {
		t.Fatal(err)
	}
	data := append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte{0}, 64)...)
	hash := sha256.Sum256(data)
	return s, frozenArtifact{ArtifactID: "artifact", SHA256: hex.EncodeToString(hash[:]), SizeBytes: int64(len(data)), MIMEType: "image/png"}, data
}
func TestProviderArtifactImportsControlledBytesAndRechecks(t *testing.T) {
	s, d, data := artifactFixture(t)
	called := 0
	a, err := s.importArtifact(context.Background(), d, bytes.NewReader(data), func(ctx context.Context, path, mime string) error {
		called++
		if filepath.Dir(path) != s.root || mime != d.MIMEType {
			t.Fatal("uncontrolled decoder input")
		}
		return nil
	})
	if err == nil && filepath.Ext(a.path()) != ".png" {
		t.Fatal("upload path lacks trusted MIME extension")
	}
	if err != nil || called != 1 {
		t.Fatal(err)
	}
	if err = a.recheck(); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(a.path(), bytes.Repeat([]byte{1}, len(data)), 0600); err != nil {
		t.Fatal(err)
	}
	if a.recheck() == nil {
		t.Fatal("accepted changed bytes")
	}
}
func TestProviderArtifactRejectsBadBytesAndDecoderFailure(t *testing.T) {
	for _, kind := range []string{"hash", "size", "mime", "decoder", "decoder changes bytes"} {
		t.Run(kind, func(t *testing.T) {
			s, d, data := artifactFixture(t)
			decode := func(context.Context, string, string) error { return nil }
			switch kind {
			case "hash":
				d.SHA256 = string(bytes.Repeat([]byte{'a'}, 64))
			case "size":
				d.SizeBytes--
			case "mime":
				d.MIMEType = "video/mp4"
			case "decoder":
				decode = func(context.Context, string, string) error { return errors.New("invalid image") }
			case "decoder changes bytes":
				decode = func(_ context.Context, p, _ string) error {
					return os.WriteFile(p, bytes.Repeat([]byte{2}, len(data)), 0600)
				}
			}
			if _, err := s.importArtifact(context.Background(), d, bytes.NewReader(data), decode); err == nil {
				t.Fatal("accepted invalid media")
			}
			entries, err := os.ReadDir(s.root)
			if err != nil || len(entries) != 0 {
				t.Fatal("failed import left file")
			}
		})
	}
}
func TestProviderArtifactRejectsLinksAndRootReplacement(t *testing.T) {
	for _, kind := range []string{"hardlink", "symlink", "root"} {
		t.Run(kind, func(t *testing.T) {
			s, d, data := artifactFixture(t)
			a, err := s.importArtifact(context.Background(), d, bytes.NewReader(data), func(context.Context, string, string) error { return nil })
			if err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "hardlink":
				if os.Link(a.path(), filepath.Join(s.root, "second")) != nil {
					t.Fatal("link")
				}
			case "symlink":
				old := a.path() + ".old"
				if os.Rename(a.path(), old) != nil || os.Symlink(old, a.path()) != nil {
					t.Fatal("replace")
				}
			case "root":
				if os.Rename(s.root, s.root+".old") != nil || os.Mkdir(s.root, 0700) != nil {
					t.Fatal("replace root")
				}
				t.Cleanup(func() { os.RemoveAll(s.root + ".old") })
			}
			if a.recheck() == nil {
				t.Fatal("accepted replaced boundary")
			}
		})
	}
}

func TestProviderArtifactLimitsAndCancellation(t *testing.T) {
	s, d, data := artifactFixture(t)
	decode := func(context.Context, string, string) error { return nil }
	for i := 0; i < 18; i++ {
		if _, err := s.importArtifact(context.Background(), d, bytes.NewReader(data), decode); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.importArtifact(context.Background(), d, bytes.NewReader(data), decode); err == nil {
		t.Fatal("accepted 19th media")
	}
	fresh, d, data := artifactFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := fresh.importArtifact(ctx, d, bytes.NewReader(data), decode); err == nil {
		t.Fatal("accepted cancelled import")
	}
	entries, _ := os.ReadDir(fresh.root)
	if len(entries) != 0 {
		t.Fatal("cancelled import created file")
	}
	d.SizeBytes = 10*1024*1024 + 1
	if _, err := fresh.importArtifact(context.Background(), d, bytes.NewReader(data), decode); err == nil {
		t.Fatal("accepted oversize descriptor")
	}
}
