package browser

import (
	"context"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

type pipeProcess struct {
	transport  *pipeTransport
	command    *exec.Cmd
	pid        int
	stop, done chan struct{}
	stopOnce   sync.Once
	cleanupErr error
}

// Owns cmd after Start. It deliberately does not reap the leader until the owned
// process group has been killed: the unreaped leader reserves its PID/PGID and
// prevents a delayed cleanup from signalling a reused unrelated process group.
func startPipeProcess(ctx context.Context, cmd *exec.Cmd) (*pipeProcess, error) {
	if ctx.Err() != nil || cmd == nil || cmd.Process != nil || len(cmd.ExtraFiles) != 0 || cmd.SysProcAttr != nil {
		return nil, errCDPPipe
	}
	commandsR, commandsW, err := os.Pipe()
	if err != nil {
		return nil, errCDPPipe
	}
	responsesR, responsesW, err := os.Pipe()
	if err != nil {
		commandsR.Close()
		commandsW.Close()
		return nil, errCDPPipe
	}
	null, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		commandsR.Close()
		commandsW.Close()
		responsesR.Close()
		responsesW.Close()
		return nil, errCDPPipe
	}
	cmd.ExtraFiles = []*os.File{commandsR, responsesW}
	cmd.Stdin = null
	cmd.Stdout = null
	cmd.Stderr = null
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	err = cmd.Start()
	commandsR.Close()
	responsesW.Close()
	null.Close()
	if err != nil {
		commandsW.Close()
		responsesR.Close()
		return nil, errCDPPipe
	}
	p := &pipeProcess{transport: newPipeTransport(responsesR, commandsW), command: cmd, pid: cmd.Process.Pid, stop: make(chan struct{}), done: make(chan struct{})}
	p.transport.onClose = p.requestStop
	lifetime, cancel := context.WithTimeout(ctx, 2*time.Minute)
	go func() {
		select {
		case <-lifetime.Done():
			p.requestStop()
		case <-p.done:
		}
	}()
	go func() {
		defer cancel()
		defer close(p.done)
		<-p.stop
		if err := syscall.Kill(-p.pid, syscall.SIGKILL); err != nil && err != syscall.ESRCH {
			p.cleanupErr = errCDPPipe
		}
		p.transport.Close()
		// Wait is called exactly once, only here, after signalling the reserved group.
		_ = cmd.Wait()
	}()
	return p, nil
}
func (p *pipeProcess) requestStop() { p.stopOnce.Do(func() { close(p.stop) }) }
func (p *pipeProcess) Close() error {
	p.requestStop()
	select {
	case <-p.done:
		return p.cleanupErr
	case <-time.After(5 * time.Second):
		return errCDPPipe
	}
}
