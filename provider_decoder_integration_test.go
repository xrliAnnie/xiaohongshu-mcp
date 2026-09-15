//go:build integration

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"image"
	"image/color"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func mediaBinaryFixture(t *testing.T, name string) mediaBinaryPin {
	t.Helper()
	path, err := exec.LookPath(name)
	if err != nil {
		t.Fatal(err)
	}
	path, err = filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	return mediaBinaryPin{Path: path, SHA256: hex.EncodeToString(sum[:])}
}
func TestProviderDecoderRealPNGAndMP4(t *testing.T) {
	root := t.TempDir()
	if os.Chmod(root, 0700) != nil {
		t.Fatal("chmod")
	}
	pngPath := filepath.Join(root, "fixture.png")
	f, err := os.OpenFile(pngPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	img := image.NewRGBA(image.Rect(0, 0, 16, 16))
	img.Set(0, 0, color.RGBA{R: 255, A: 255})
	if png.Encode(f, img) != nil || f.Close() != nil {
		t.Fatal("png fixture")
	}
	ffmpeg, ffprobe := mediaBinaryFixture(t, "ffmpeg"), mediaBinaryFixture(t, "ffprobe")
	decoder, err := newProviderMediaDecoder(ffmpeg, ffprobe, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err = decoder(context.Background(), pngPath, "image/png"); err != nil {
		t.Fatal("real png rejected", err)
	}

	storeRoot := filepath.Join(root, "store")
	if os.Mkdir(storeRoot, 0700) != nil {
		t.Fatal("store root")
	}
	store, err := newProviderArtifactStore(storeRoot)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(pngPath)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	artifact, err := store.importArtifact(context.Background(), frozenArtifact{ArtifactID: "actual-png", SHA256: hex.EncodeToString(sum[:]), SizeBytes: int64(len(data)), MIMEType: "image/png"}, bytes.NewReader(data), decoder)
	if err != nil || artifact.recheck() != nil {
		t.Fatal("actual import+decode failed", err)
	}
	if err = decoder(context.Background(), pngPath, "image/jpeg"); err == nil {
		t.Fatal("accepted wrong decoder")
	}
	mp4Path := filepath.Join(root, "fixture.mp4")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, ffmpeg.Path, "-nostdin", "-v", "error", "-f", "lavfi", "-i", "color=c=red:s=16x16:d=0.1", "-c:v", "mpeg4", "-threads", "1", mp4Path)
	cmd.Dir = root
	cmd.Env = []string{"PATH=/usr/bin:/bin", "LANG=C", "LC_ALL=C", "HOME=" + root}
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("synthetic video generation: %v %s", err, output)
	}
	if os.Chmod(mp4Path, 0600) != nil {
		t.Fatal("chmod mp4")
	}
	if err = decoder(context.Background(), mp4Path, "video/mp4"); err != nil {
		t.Fatal("real mp4 rejected", err)
	}
	if os.WriteFile(pngPath, []byte("\x89PNG\r\n\x1a\ninvalid"), 0600) != nil {
		t.Fatal("corrupt")
	}
	if err = decoder(context.Background(), pngPath, "image/png"); err == nil {
		t.Fatal("accepted magic-only corrupt image")
	}
	canceled, stop := context.WithCancel(context.Background())
	stop()
	if err = decoder(canceled, mp4Path, "video/mp4"); err == nil {
		t.Fatal("accepted canceled")
	}
	ffprobe.SHA256 = string(make([]byte, 64))
	if _, err = newProviderMediaDecoder(ffmpeg, ffprobe, time.Second); err == nil {
		t.Fatal("accepted unpinned probe")
	}
}
