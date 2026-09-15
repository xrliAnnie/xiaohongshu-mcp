package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"
)

type mediaBinaryPin struct{ Path, SHA256 string }

// Startup policy must additionally prove root-owned immutable installation and
// ancestor permissions. This per-call check detects drift against those fixed pins.
func verifyMediaBinary(pin mediaBinaryPin) error {
	if !filepath.IsAbs(pin.Path) || filepath.Clean(pin.Path) != pin.Path || !journalDigest.MatchString(pin.SHA256) {
		return errProviderArtifact
	}
	fd, err := unix.Open(pin.Path, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return errProviderArtifact
	}
	f := os.NewFile(uintptr(fd), "media-decoder")
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0022 != 0 || info.Mode().Perm()&0111 == 0 || info.Size() > 1024*1024*1024 {
		return errProviderArtifact
	}
	hash := sha256.New()
	if _, err = io.Copy(hash, io.LimitReader(f, 1024*1024*1024+1)); err != nil || hex.EncodeToString(hash.Sum(nil)) != pin.SHA256 {
		return errProviderArtifact
	}
	after, err := os.Lstat(pin.Path)
	if err != nil || !os.SameFile(info, after) || !info.ModTime().Equal(after.ModTime()) {
		return errProviderArtifact
	}
	return nil
}

type decoderOutput struct{ buffer bytes.Buffer }

func (b *decoderOutput) Write(data []byte) (int, error) {
	if b.buffer.Len()+len(data) > 65536 {
		return 0, errProviderArtifact
	}
	return b.buffer.Write(data)
}
func validDecoderProbe(raw []byte, mime string) bool {
	var response struct {
		Streams []struct {
			Type   string `json:"codec_type"`
			Codec  string `json:"codec_name"`
			Width  int64  `json:"width"`
			Height int64  `json:"height"`
		} `json:"streams"`
	}
	if len(raw) > 65536 || json.Unmarshal(raw, &response) != nil || len(response.Streams) < 1 || len(response.Streams) > 2 {
		return false
	}
	videos := 0
	for _, s := range response.Streams {
		if s.Type == "audio" {
			if mime != "video/mp4" {
				return false
			}
			continue
		}
		if s.Type != "video" || s.Width < 1 || s.Height < 1 || s.Width > 40000000/s.Height {
			return false
		}
		videos++
		if mime != "video/mp4" {
			expected := map[string]string{"image/png": "png", "image/jpeg": "mjpeg", "image/webp": "webp"}[mime]
			if expected == "" || s.Codec != expected || len(response.Streams) != 1 {
				return false
			}
		}
	}
	return videos == 1
}
func newProviderMediaDecoder(ffmpeg, ffprobe mediaBinaryPin, timeout time.Duration) (func(context.Context, string, string) error, error) {
	if timeout <= 0 || timeout > 60*time.Second || verifyMediaBinary(ffmpeg) != nil || verifyMediaBinary(ffprobe) != nil {
		return nil, errProviderArtifact
	}
	return func(parent context.Context, path, mime string) error {
		ctx, cancel := context.WithTimeout(parent, timeout)
		defer cancel()
		if ctx.Err() != nil || !filepath.IsAbs(path) || filepath.Clean(path) != path {
			return errProviderArtifact
		}
		file, err := os.Lstat(path)
		root, rootErr := os.Lstat(filepath.Dir(path))
		if err != nil || rootErr != nil || !privateJournalEntry(root, true) || file.Size() < 1 || file.Size() > 10*1024*1024 || !providerMediaFile(file, file.Size()) {
			return errProviderArtifact
		}
		demuxer := map[string]string{"image/png": "png_pipe", "image/jpeg": "jpeg_pipe", "image/webp": "webp_pipe", "video/mp4": "mov"}[mime]
		if demuxer == "" {
			return errProviderArtifact
		}
		input := []string{"-protocol_whitelist", "file,pipe", "-f", demuxer}
		if mime == "video/mp4" {
			input = append(input, "-enable_drefs", "0", "-use_absolute_path", "0")
		}
		input = append(input, "-i", path)
		run := func(pin mediaBinaryPin, args []string) ([]byte, error) {
			if ctx.Err() != nil || verifyMediaBinary(pin) != nil {
				return nil, errProviderArtifact
			}
			cmd := exec.CommandContext(ctx, pin.Path, args...)
			cmd.Dir = filepath.Dir(path)
			cmd.Env = []string{"PATH=/usr/bin:/bin", "LANG=C", "LC_ALL=C", "HOME=" + cmd.Dir, "TMPDIR=" + cmd.Dir}
			cmd.WaitDelay = time.Second
			var stdout, stderr decoderOutput
			cmd.Stdout = &stdout
			cmd.Stderr = &stderr
			if cmd.Run() != nil || ctx.Err() != nil {
				return nil, errProviderArtifact
			}
			return stdout.buffer.Bytes(), nil
		}
		probeArgs := append([]string{"-v", "error", "-max_alloc", "67108864"}, input...)
		probeArgs = append(probeArgs, "-show_entries", "stream=codec_type,codec_name,width,height", "-of", "json")
		probe, err := run(ffprobe, probeArgs)
		if err != nil || !validDecoderProbe(probe, mime) {
			return errProviderArtifact
		}
		args := append([]string{"-nostdin", "-v", "error", "-xerror", "-max_alloc", "67108864", "-err_detect", "explode", "-threads", "1"}, input...)
		args = append(args, "-map", "0:v:0", "-map", "0:a?", "-threads", "1", "-filter_threads", "1", "-f", "null", "-")
		if _, err = run(ffmpeg, args); err != nil {
			return errProviderArtifact
		}
		return nil
	}, nil
}
