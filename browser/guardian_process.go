package browser

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Called only after root binary verification and durable profile creation.
// The provider owns the sole liveness writer; cancellation closes that writer.
// Only the guardian may kill/reap the browser group, including on provider death.
func startGuardianProcess(ctx context.Context, browser *exec.Cmd, guardian string, profile *guardianProfile, budget time.Duration) (*pipeProcess, error) {
	if ctx.Err() != nil || budget < time.Millisecond || budget > 4*time.Minute || browser == nil || browser.Process != nil || len(browser.Args) == 0 || len(browser.Env) == 0 || !filepath.IsAbs(browser.Path) || browser.SysProcAttr != nil || len(browser.ExtraFiles) != 0 || profile == nil || profile.receipt == nil || !filepath.IsAbs(guardian) || filepath.Clean(guardian) != guardian {
		return nil, errCDPPipe
	}
	var files []*os.File
	retained := map[*os.File]bool{}
	defer func() {
		for _, file := range files {
			if !retained[file] {
				file.Close()
			}
		}
	}()
	pipe := func() (*os.File, *os.File, error) {
		r, w, err := os.Pipe()
		if err == nil {
			files = append(files, r, w)
		}
		return r, w, err
	}
	commandR, commandW, err := pipe()
	if err != nil {
		return nil, errCDPPipe
	}
	responseR, responseW, err := pipe()
	if err != nil {
		return nil, errCDPPipe
	}
	liveR, liveW, err := pipe()
	if err != nil {
		return nil, errCDPPipe
	}
	statusR, statusW, err := pipe()
	if err != nil {
		return nil, errCDPPipe
	}
	args := []string{strconv.FormatInt(budget.Milliseconds(), 10), profile.identity.Nonce, browser.Path}
	args = append(args, browser.Args[1:]...)
	command := exec.Command(guardian, args...)
	// Provider-group teardown must leave the guardian alive to observe EOF.
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Dir = browser.Dir
	command.Env = append([]string(nil), browser.Env...)
	command.ExtraFiles = []*os.File{commandR, responseW, liveR, profile.receipt, statusW}
	if err := command.Start(); err != nil {
		return nil, errCDPPipe
	}
	// A non-nil owner is retained for every failure after Start.
	commandR.Close()
	responseW.Close()
	liveR.Close()
	statusW.Close()
	profile.receipt.Close()
	retained[commandW] = true
	retained[responseR] = true
	retained[liveW] = true
	p := &pipeProcess{transport: newPipeTransport(responseR, commandW), command: command, stop: make(chan struct{}), done: make(chan struct{})}
	p.transport.onClose = p.requestStop
	lifetime, cancel := context.WithTimeout(ctx, budget)
	ready := make(chan struct{})
	stopped := make(chan struct{})
	go func() {
		select {
		case <-lifetime.Done():
			p.requestStop()
		case <-p.done:
		}
	}()
	go func() { defer close(stopped); <-p.stop; liveW.Close(); p.transport.Close() }()
	go func() {
		defer cancel()
		defer close(p.done)
		waitErr := command.Wait()
		<-ready
		p.requestStop()
		<-stopped
		if waitErr != nil {
			exit, ok := waitErr.(*exec.ExitError)
			if !ok || exit.ExitCode() != 71 {
				p.cleanupErr = errCDPPipe
				return
			}
		}
		completed, err := loadCompletedGuardianProfile(profile.root, filepath.Base(profile.path), profile.identity.Scope)
		if err != nil || completed.identity != profile.identity {
			p.cleanupErr = errCDPPipe
			return
		}
		var receipt guardianCleanupReceipt
		if readGuardianRecord(filepath.Join(profile.path, "cleanup.json"), &receipt) != nil || (p.pid > 0 && receipt.PID != p.pid) {
			p.cleanupErr = errCDPPipe
		}
	}()
	defer close(ready)
	deadline := time.Now().Add(5 * time.Second)
	if until, ok := lifetime.Deadline(); ok && until.Before(deadline) {
		deadline = until
	}
	if statusR.SetReadDeadline(deadline) != nil {
		p.requestStop()
		return p, errCDPPipe
	}
	line, err := bufio.NewReader(io.LimitReader(statusR, 64)).ReadString('\n')
	if err != nil || len(line) > 63 {
		p.requestStop()
		return p, errCDPPipe
	}
	value := strings.TrimSuffix(strings.TrimPrefix(line, "START "), "\n")
	pid, err := strconv.Atoi(value)
	if err != nil || pid <= 0 || pid > 2147483647 || line != fmt.Sprintf("START %d\n", pid) {
		p.requestStop()
		return p, errCDPPipe
	}
	p.pid = pid
	if ctx.Err() != nil {
		p.requestStop()
		return p, errCDPPipe
	}
	return p, nil
}
