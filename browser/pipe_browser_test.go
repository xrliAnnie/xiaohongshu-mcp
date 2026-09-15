package browser

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func pipeBrowserOptions(t *testing.T) PipeBrowserOptions {
	t.Helper()
	binary, err := exec.LookPath("sh")
	if err != nil {
		t.Fatal(err)
	}
	binary, err = filepath.EvalSymlinks(binary)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(binary)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	return PipeBrowserOptions{BinaryPath: binary, BinarySHA256: hex.EncodeToString(sum[:]), ProfileRoot: root}
}
func TestPipeBrowserCommandHasNoTCPOrInheritedCredentials(t *testing.T) {
	options := pipeBrowserOptions(t)
	t.Setenv("HTTP_PROXY", "synthetic-secret")
	t.Setenv("ROD_BROWSER_WS", "ws://unsafe")
	cmd, profile, err := preparePipeCommand(options)
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(profile)
	joined := strings.Join(cmd.Args, " ")
	if !strings.Contains(joined, "--remote-debugging-pipe") || !strings.Contains(joined, "--headless=new") || strings.Contains(joined, "remote-debugging-port") {
		t.Fatal("not exclusive headless pipe command")
	}
	if !strings.Contains(joined, "--user-data-dir="+profile) {
		t.Fatal("missing private profile")
	}
	for _, value := range cmd.Env {
		if strings.Contains(value, "synthetic-secret") || strings.HasPrefix(value, "HTTP_PROXY=") || strings.HasPrefix(value, "ROD_") {
			t.Fatal("inherited environment")
		}
	}
	info, err := os.Lstat(profile)
	if err != nil || info.Mode().Perm() != 0700 {
		t.Fatal("profile not private")
	}
}
func TestPipeBrowserRejectsDriftAndCleansFailedConnect(t *testing.T) {
	options := pipeBrowserOptions(t)
	bad := options
	bad.BinarySHA256 = strings.Repeat("0", 64)
	if _, _, err := preparePipeCommand(bad); err == nil {
		t.Fatal("accepted binary drift")
	}
	entries, _ := os.ReadDir(options.ProfileRoot)
	if len(entries) != 0 {
		t.Fatal("created profile before pin verification")
	}
	// Exercise the real CDP failure/cleanup path with a live, owned child.
	// The separate command test above verifies the immutable executable policy.
	profile, err := os.MkdirTemp(options.ProfileRoot, "lease-")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestPipeProcessHelper$")
	cmd.Env = []string{"XHS_TEST_PIPE_HELPER=1", "XHS_TEST_PIPE_REJECT=1"}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := launchPreparedPipeBrowser(ctx, cmd, profile); err == nil {
		t.Fatal("accepted non-CDP executable")
	}
	entries, _ = os.ReadDir(options.ProfileRoot)
	if len(entries) != 0 {
		t.Fatal("failed startup left profile")
	}
}

func TestPipeBrowserPreservesProfileWhenCleanupIsUnconfirmed(t *testing.T) {
	profile := t.TempDir()
	info, err := os.Lstat(profile)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	close(done)
	process := &pipeProcess{stop: make(chan struct{}), done: done, cleanupErr: errCDPPipe}
	browser := &PipeBrowser{process: process, profile: profile, profileInfo: info, cancel: func() {}}
	retained, failure := failedPipeStartup(browser)
	if retained != browser || failure == nil {
		t.Fatal("lost ownership after failed cleanup")
	}
	if browser.Close() == nil {
		t.Fatal("unconfirmed cleanup reported success")
	}
	current, err := os.Lstat(profile)
	if err != nil || !os.SameFile(info, current) {
		t.Fatal("removed profile despite unconfirmed process cleanup")
	}
}

func TestPipeBrowserRequiresGuardianPinAndScopeBeforeCreatingProfile(t *testing.T) {
	options := pipeBrowserOptions(t)
	for _, mode := range []string{"missing", "digest", "scope"} {
		candidate := options
		candidate.Scope = guardianScopeFixture
		if mode != "missing" {
			candidate.GuardianPath = options.BinaryPath
			candidate.GuardianSHA256 = options.BinarySHA256
		}
		if mode == "digest" {
			candidate.GuardianSHA256 = strings.Repeat("0", 64)
		}
		if mode == "scope" {
			candidate.Scope = GuardianProfileScope{}
		}
		owner, err := LaunchPipeBrowser(context.Background(), candidate)
		if owner != nil {
			defer owner.Close()
		}
		if err == nil || owner != nil {
			t.Fatal("unsafe production launch", mode)
		}
		if entries, _ := os.ReadDir(options.ProfileRoot); len(entries) != 0 {
			t.Fatal("created profile before authority verification", mode)
		}
	}
}
