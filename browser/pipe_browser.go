package browser

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/cdp"
	"golang.org/x/sys/unix"
)

type PipeBrowserOptions struct{ BinaryPath, BinarySHA256, ProfileRoot string }
type PipeBrowser struct {
	Browser     *rod.Browser
	process     *pipeProcess
	profile     string
	profileInfo os.FileInfo
	cancel      context.CancelFunc
	once        sync.Once
	closeErr    error
}

// The executable and every parent must be root-owned and not writable by the
// dedicated service/model users. Policy pins the immutable installation's digest.
func verifyPipeBinary(path, digest string) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || len(digest) != 64 {
		return errCDPPipe
	}
	if _, err := hex.DecodeString(digest); err != nil {
		return errCDPPipe
	}
	for current := path; ; current = filepath.Dir(current) {
		info, err := os.Lstat(current)
		if err != nil {
			return errCDPPipe
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || stat.Uid != 0 || info.Mode().Perm()&0022 != 0 || info.Mode()&os.ModeSymlink != 0 {
			return errCDPPipe
		}
		if current == path {
			if !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 || info.Size() > 1024*1024*1024 {
				return errCDPPipe
			}
		} else if !info.IsDir() {
			return errCDPPipe
		}
		if filepath.Dir(current) == current {
			break
		}
	}
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return errCDPPipe
	}
	file := os.NewFile(uintptr(fd), "pinned-browser")
	defer file.Close()
	hash := sha256.New()
	if _, err = io.Copy(hash, file); err != nil || hex.EncodeToString(hash.Sum(nil)) != digest {
		return errCDPPipe
	}
	opened, err := file.Stat()
	current, pathErr := os.Lstat(path)
	if err != nil || pathErr != nil || !os.SameFile(opened, current) {
		return errCDPPipe
	}
	return nil
}
func preparePipeCommand(options PipeBrowserOptions) (*exec.Cmd, string, error) {
	if err := verifyPipeBinary(options.BinaryPath, options.BinarySHA256); err != nil {
		return nil, "", err
	}
	root, err := os.Lstat(options.ProfileRoot)
	if err != nil || !filepath.IsAbs(options.ProfileRoot) || !root.IsDir() || root.Mode().Perm() != 0700 {
		return nil, "", errCDPPipe
	}
	owner, ok := root.Sys().(*syscall.Stat_t)
	if !ok || owner.Uid != uint32(os.Geteuid()) {
		return nil, "", errCDPPipe
	}
	profile, err := os.MkdirTemp(options.ProfileRoot, "lease-")
	if err != nil {
		return nil, "", errCDPPipe
	}
	cmd := exec.Command(options.BinaryPath,
		"--headless=new", "--remote-debugging-pipe", "--user-data-dir="+profile,
		"--no-first-run", "--no-default-browser-check", "--disable-background-networking",
		"--disable-component-update", "--disable-sync", "--disable-extensions",
		"--disable-breakpad", "--disable-crash-reporter", "--noerrdialogs",
		"--password-store=basic", "--use-mock-keychain", "about:blank")
	cmd.Env = []string{"PATH=/usr/bin:/bin", "LANG=C", "LC_ALL=C", "HOME=" + profile, "TMPDIR=" + profile}
	cmd.Dir = profile
	return cmd, profile, nil
}
func LaunchPipeBrowser(ctx context.Context, options PipeBrowserOptions) (*PipeBrowser, error) {
	if ctx.Err() != nil {
		return nil, errCDPPipe
	}
	cmd, profile, err := preparePipeCommand(options)
	if err != nil {
		return nil, err
	}
	return launchPreparedPipeBrowser(ctx, cmd, profile)
}

// Only LaunchPipeBrowser supplies commands in production, after policy verification.
func launchPreparedPipeBrowser(ctx context.Context, cmd *exec.Cmd, profile string) (*PipeBrowser, error) {
	info, err := os.Lstat(profile)
	if err != nil {
		os.RemoveAll(profile)
		return nil, errCDPPipe
	}
	lifetime, cancel := context.WithCancel(ctx)
	process, err := startPipeProcess(lifetime, cmd)
	if err != nil {
		cancel()
		os.RemoveAll(profile)
		return nil, err
	}
	owned := &PipeBrowser{process: process, profile: profile, profileInfo: info, cancel: cancel}
	quiet := log.New(io.Discard, "", 0)
	client := cdp.New().Logger(quiet).Start(process.transport)
	owned.Browser = rod.New().Context(lifetime).ControlURL("").Monitor("").Trace(false).SlowMotion(0).Logger(quiet).Client(client)
	if err = owned.Browser.Connect(); err != nil {
		owned.Close()
		return nil, errCDPPipe
	}
	return owned, nil
}
func (b *PipeBrowser) PID() int { return b.process.pid }
func (b *PipeBrowser) Close() error {
	b.once.Do(func() {
		b.cancel()
		if err := b.process.Close(); err != nil {
			b.closeErr = err
			return
		}
		current, err := os.Lstat(b.profile)
		if err != nil || !os.SameFile(b.profileInfo, current) {
			b.closeErr = errCDPPipe
			return
		}
		b.closeErr = os.RemoveAll(b.profile)
	})
	return b.closeErr
}
