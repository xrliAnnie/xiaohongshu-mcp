//go:build integration && darwin

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/xpzouying/xiaohongshu-mcp/configs"
	"golang.org/x/sys/unix"
)

const (
	processCaptureTimeout = 30 * time.Second
	searchReturnTimeout   = 120 * time.Second
	processExitTimeout    = 20 * time.Second
)

type serviceCallOutcome struct {
	elapsed time.Duration
	err     error
	panic   any
}

type processIdentity struct {
	pid     int
	ppid    int
	lstart  string
	comm    string
	command string
}

func TestTimeoutE2E(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte("<!doctype html><html><body>stable fixture without initial state</body></html>"))
	}))
	t.Cleanup(server.Close)
	configureBrowserTestEnvironment(t, server.URL)
	existingProfiles := snapshotRodProfiles(t)

	outcomeCh := make(chan serviceCallOutcome, 1)
	start := time.Now()
	go func() {
		outcome := serviceCallOutcome{}
		defer func() {
			outcome.elapsed = time.Since(start)
			outcome.panic = recover()
			outcomeCh <- outcome
		}()

		_, outcome.err = NewXiaohongshuService().SearchFeeds(context.Background(), "timeout-fixture", 1)
	}()

	lineage := captureRodLineage(t, os.Getpid(), existingProfiles, outcomeCh, processCaptureTimeout)
	outcome := waitForServiceOutcome(t, outcomeCh, searchReturnTimeout)
	t.Logf("SearchFeeds finished after %s (panic=%v, err=%v)", outcome.elapsed, outcome.panic, outcome.err)

	if outcome.panic == nil && outcome.err == nil {
		t.Fatal("SearchFeeds unexpectedly succeeded against a page that never exposes __INITIAL_STATE__")
	}
	if outcome.elapsed > searchReturnTimeout {
		t.Fatalf("SearchFeeds exceeded the %s return budget: %s", searchReturnTimeout, outcome.elapsed)
	}

	waitForExactProcessExit(t, lineage, processExitTimeout)
}

func TestConstructorStall(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			<-r.Context().Done()
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(server.Close)
	configureBrowserTestEnvironment(t, server.URL)
	existingProfiles := snapshotRodProfiles(t)

	outcomeCh := make(chan serviceCallOutcome, 1)
	start := time.Now()
	go func() {
		outcome := serviceCallOutcome{}
		defer func() {
			outcome.elapsed = time.Since(start)
			outcome.panic = recover()
			outcomeCh <- outcome
		}()

		_, outcome.err = NewXiaohongshuService().ListFeeds(context.Background())
	}()

	lineage := captureRodLineage(t, os.Getpid(), existingProfiles, outcomeCh, processCaptureTimeout)
	outcome := waitForServiceOutcome(t, outcomeCh, searchReturnTimeout)
	t.Logf("ListFeeds constructor finished after %s (panic=%v, err=%v)", outcome.elapsed, outcome.panic, outcome.err)

	if outcome.panic == nil && outcome.err == nil {
		t.Fatal("ListFeeds unexpectedly succeeded while the constructor navigation never completed")
	}
	waitForExactProcessExit(t, lineage, processExitTimeout)
}

func TestQrcodeCleanup(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte("<!doctype html><html><body>login fixture without a QR element</body></html>"))
	}))
	t.Cleanup(server.Close)
	configureBrowserTestEnvironment(t, server.URL)
	existingProfiles := snapshotRodProfiles(t)

	outcomeCh := make(chan serviceCallOutcome, 1)
	start := time.Now()
	go func() {
		outcome := serviceCallOutcome{}
		defer func() {
			outcome.elapsed = time.Since(start)
			outcome.panic = recover()
			outcomeCh <- outcome
		}()

		_, outcome.err = NewXiaohongshuService().GetLoginQrcode(context.Background())
	}()

	lineage := captureRodLineage(t, os.Getpid(), existingProfiles, outcomeCh, processCaptureTimeout)
	outcome := waitForServiceOutcome(t, outcomeCh, searchReturnTimeout)
	t.Logf("GetLoginQrcode finished after %s (panic=%v, err=%v)", outcome.elapsed, outcome.panic, outcome.err)

	if outcome.panic == nil {
		t.Fatalf("GetLoginQrcode must panic on its rod deadline in this direct service test; err=%v", outcome.err)
	}
	waitForExactProcessExit(t, lineage, processExitTimeout)
}

func configureBrowserTestEnvironment(t *testing.T, baseURL string) {
	t.Helper()

	oldHeadless := configs.IsHeadless()
	oldBinPath := configs.GetBinPath()
	oldBaseURL, hadBaseURL := os.LookupEnv("XHS_BASE_URL")
	oldProxy, hadProxy := os.LookupEnv("XHS_PROXY")
	oldCookiesPath, hadCookiesPath := os.LookupEnv("COOKIES_PATH")

	t.Cleanup(func() {
		restoreEnv("XHS_BASE_URL", oldBaseURL, hadBaseURL)
		restoreEnv("XHS_PROXY", oldProxy, hadProxy)
		restoreEnv("COOKIES_PATH", oldCookiesPath, hadCookiesPath)
		configs.InitBaseURL()
		configs.InitHeadless(oldHeadless)
		configs.SetBinPath(oldBinPath)
	})

	if err := os.Setenv("XHS_BASE_URL", baseURL); err != nil {
		t.Fatalf("set XHS_BASE_URL: %v", err)
	}
	if err := os.Setenv("XHS_PROXY", ""); err != nil {
		t.Fatalf("clear XHS_PROXY: %v", err)
	}
	if err := os.Setenv("COOKIES_PATH", filepath.Join(t.TempDir(), "cookies.json")); err != nil {
		t.Fatalf("set COOKIES_PATH: %v", err)
	}

	configs.InitBaseURL()
	configs.InitHeadless(true)
	configs.SetBinPath(integrationBrowserBin(t))
}

func integrationBrowserBin(t *testing.T) string {
	t.Helper()

	if configured := os.Getenv("XHS_TEST_BROWSER_BIN"); configured != "" {
		if _, err := os.Stat(configured); err != nil {
			t.Fatalf("XHS_TEST_BROWSER_BIN is not executable: %v", err)
		}
		return configured
	}

	for _, candidate := range []string{
		"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
		"/Applications/Chromium.app/Contents/MacOS/Chromium",
	} {
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}

	t.Fatal("integration test requires XHS_TEST_BROWSER_BIN or an installed Chrome/Chromium")
	return ""
}

func restoreEnv(key, value string, existed bool) {
	if existed {
		_ = os.Setenv(key, value)
		return
	}
	_ = os.Unsetenv(key)
}

func waitForServiceOutcome(t *testing.T, outcomeCh <-chan serviceCallOutcome, timeout time.Duration) serviceCallOutcome {
	t.Helper()

	select {
	case outcome := <-outcomeCh:
		return outcome
	case <-time.After(timeout):
		t.Fatalf("SearchFeeds did not return within %s", timeout)
		return serviceCallOutcome{}
	}
}

func captureRodLineage(t *testing.T, rootPID int, existingProfiles map[string]struct{}, outcomeCh <-chan serviceCallOutcome, timeout time.Duration) []processIdentity {
	t.Helper()

	deadline := time.Now().Add(timeout)
	lastIssue := "no new rod profile observed"
	for time.Now().Before(deadline) {
		select {
		case outcome := <-outcomeCh:
			t.Fatalf("SearchFeeds finished before rod lineage capture after %s (panic=%v, err=%v)", outcome.elapsed, outcome.panic, outcome.err)
		default:
		}

		profiles, err := filepath.Glob(filepath.Join(os.TempDir(), "rod", "user-data", "*"))
		if err != nil {
			t.Fatalf("list rod profiles: %v", err)
		}
		for _, profile := range profiles {
			if _, existed := existingProfiles[profile]; existed {
				continue
			}
			lastIssue = "new rod profile observed without a readable SingletonLock"

			pid, pidErr := chromiumPIDFromProfile(profile)
			if pidErr != nil {
				continue
			}
			lastIssue = fmt.Sprintf("SingletonLock exposed pid=%d but identity lookup did not complete", pid)
			process, processErr := processIdentityForPID(pid)
			if processErr != nil {
				lastIssue = fmt.Sprintf("pid=%d identity lookup failed: %v", pid, processErr)
				continue
			}
			if !isRodBrowserMain(process) {
				lastIssue = fmt.Sprintf("pid=%d did not match rod main classifier (comm=%q command_sha256=%s)", pid, process.comm, commandHash(process.command))
				continue
			}
			if !isDescendantOf(process.pid, rootPID) {
				lastIssue = fmt.Sprintf("pid=%d was not a descendant of test pid=%d", pid, rootPID)
				continue
			}

			guard, guardErr := processIdentityForPID(process.ppid)
			if guardErr != nil || !isDescendantOf(guard.pid, rootPID) || !strings.Contains(strings.ToLower(guard.command), "leakless") {
				t.Fatalf("captured rod Chromium pid=%d without its test-owned leakless parent: %v", process.pid, guardErr)
			}

			t.Logf(
				"captured rod lineage: chromium_pid=%d chromium_command_sha256=%s leakless_pid=%d leakless_command_sha256=%s",
				process.pid,
				commandHash(process.command),
				guard.pid,
				commandHash(guard.command),
			)
			return []processIdentity{process, guard}
		}

		time.Sleep(100 * time.Millisecond)
	}

	t.Fatalf("did not capture a test-owned rod Chromium main process within %s: %s", timeout, lastIssue)
	return nil
}

func snapshotRodProfiles(t *testing.T) map[string]struct{} {
	t.Helper()

	profiles, err := filepath.Glob(filepath.Join(os.TempDir(), "rod", "user-data", "*"))
	if err != nil {
		t.Fatalf("list existing rod profiles: %v", err)
	}
	result := make(map[string]struct{}, len(profiles))
	for _, profile := range profiles {
		result[profile] = struct{}{}
	}
	return result
}

func chromiumPIDFromProfile(profile string) (int, error) {
	target, err := os.Readlink(filepath.Join(profile, "SingletonLock"))
	if err != nil {
		return 0, err
	}
	separator := strings.LastIndexByte(target, '-')
	if separator < 0 || separator == len(target)-1 {
		return 0, fmt.Errorf("unexpected SingletonLock target")
	}
	return strconv.Atoi(target[separator+1:])
}

func processIdentityForPID(pid int) (processIdentity, error) {
	info, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return processIdentity{}, err
	}
	command, err := processCommand(pid)
	if err != nil {
		return processIdentity{}, err
	}
	return processIdentity{
		pid:     pid,
		ppid:    int(info.Eproc.Ppid),
		lstart:  fmt.Sprintf("%d.%06d", info.Proc.P_starttime.Sec, info.Proc.P_starttime.Usec),
		comm:    cString(info.Proc.P_comm[:]),
		command: command,
	}, nil
}

func processCommand(pid int) (string, error) {
	raw, err := unix.SysctlRaw("kern.procargs2", pid)
	if err != nil {
		return "", err
	}
	if len(raw) < 4 {
		return "", fmt.Errorf("kern.procargs2 returned %d bytes", len(raw))
	}

	argc := int(binary.LittleEndian.Uint32(raw[:4]))
	data := raw[4:]
	executableEnd := bytes.IndexByte(data, 0)
	if executableEnd < 0 {
		return "", fmt.Errorf("kern.procargs2 omitted executable terminator")
	}
	data = data[executableEnd+1:]
	data = bytes.TrimLeft(data, "\x00")

	args := make([]string, 0, argc)
	for len(data) > 0 && len(args) < argc {
		end := bytes.IndexByte(data, 0)
		if end < 0 {
			break
		}
		if end > 0 {
			args = append(args, string(data[:end]))
		}
		data = data[end+1:]
	}
	if len(args) == 0 {
		return "", fmt.Errorf("kern.procargs2 omitted argv for pid %d", pid)
	}
	return strings.Join(args, " "), nil
}

func cString(value []byte) string {
	if end := bytes.IndexByte(value, 0); end >= 0 {
		value = value[:end]
	}
	return string(value)
}

func isRodBrowserMain(process processIdentity) bool {
	command := process.command
	if strings.Contains(command, "--type=") {
		return false
	}
	if !strings.Contains(command, "/rod/user-data/") && !strings.Contains(command, "/.cache/rod/browser/") {
		return false
	}
	comm := strings.ToLower(process.comm)
	return strings.Contains(comm, "chromium") || strings.Contains(comm, "chrome")
}

func isDescendantOf(pid, rootPID int) bool {
	seen := map[int]struct{}{}
	for pid > 0 {
		if pid == rootPID {
			return true
		}
		if _, exists := seen[pid]; exists {
			return false
		}
		seen[pid] = struct{}{}

		process, err := processIdentityForPID(pid)
		if err != nil {
			return false
		}
		pid = process.ppid
	}
	return false
}

func waitForExactProcessExit(t *testing.T, identities []processIdentity, timeout time.Duration) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		remaining := 0
		for _, identity := range identities {
			current, err := processIdentityForPID(identity.pid)
			if err == nil && current.lstart == identity.lstart && current.comm == identity.comm && current.command == identity.command {
				remaining++
			}
		}
		if remaining == 0 {
			return
		}

		time.Sleep(100 * time.Millisecond)
	}

	remaining := make([]string, 0, len(identities))
	for _, identity := range identities {
		current, err := processIdentityForPID(identity.pid)
		if err == nil && current.lstart == identity.lstart && current.comm == identity.comm && current.command == identity.command {
			remaining = append(remaining, fmt.Sprintf("pid=%d command_sha256=%s", identity.pid, commandHash(identity.command)))
		}
	}

	t.Fatalf("test-owned rod processes still matched their exact identities after %s: %s", timeout, strings.Join(remaining, ", "))
}

func commandHash(command string) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(command)))
}
