package browser

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"
)

// This helper uses only inherited anonymous pipes, never a socket or real browser profile.
func TestPipeProcessHelper(t *testing.T) {
	if os.Getenv("XHS_TEST_PIPE_HELPER") != "1" {
		return
	}
	peer := newPipeTransport(os.NewFile(3, "commands"), os.NewFile(4, "responses"))
	data, err := peer.Read()
	if err != nil {
		os.Exit(2)
	}
	var request struct {
		ID int `json:"id"`
	}
	if json.Unmarshal(data, &request) != nil {
		os.Exit(3)
	}
	var descendant *exec.Cmd
	if os.Getenv("XHS_TEST_PIPE_DESCENDANT") == "1" {
		descendant = exec.Command("/bin/sleep", "30")
		if descendant.Start() != nil {
			os.Exit(4)
		}
	}
	childPID := 0
	if descendant != nil {
		childPID = descendant.Process.Pid
	}
	response := []byte(fmt.Sprintf(`{"id":%d,"result":{"pid":%d,"child":%d}}`, request.ID, os.Getpid(), childPID))
	if os.Getenv("XHS_TEST_PIPE_REJECT") == "1" {
		response = []byte(fmt.Sprintf(`{"id":%d,"error":{"code":-32601,"message":"synthetic rejection"}}`, request.ID))
	}
	if peer.Send(response) != nil {
		os.Exit(5)
	}
	for {
		time.Sleep(time.Second)
	}
}
func TestPipeProcessHandsOffFDsAndClosesExactGroup(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.Command(os.Args[0], "-test.run=^TestPipeProcessHelper$")
	cmd.Env = []string{"XHS_TEST_PIPE_HELPER=1", "XHS_TEST_PIPE_DESCENDANT=1", "PATH=/usr/bin:/bin"}
	process, err := startPipeProcess(ctx, cmd)
	if err != nil {
		t.Fatal(err)
	}
	defer process.Close()
	if err = process.transport.Send([]byte(`{"id":1}`)); err != nil {
		t.Fatal(err)
	}
	response, err := process.transport.Read()
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		Result struct {
			PID   int `json:"pid"`
			Child int `json:"child"`
		} `json:"result"`
	}
	if json.Unmarshal(response, &result) != nil {
		t.Fatal("bad response")
	}
	if result.Result.PID != process.pid || result.Result.Child <= 0 {
		t.Fatal("nonempty exact lineage missing")
	}
	if err = process.Close(); err != nil {
		t.Fatal(err)
	}
	if syscall.Kill(process.pid, 0) == nil {
		t.Fatal("exact child still exists")
	}
	// Reparented descendants may briefly remain zombies; bounded wait for their exit.
	deadline := time.Now().Add(2 * time.Second)
	for syscall.Kill(result.Result.Child, 0) == nil && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if syscall.Kill(result.Result.Child, 0) == nil {
		t.Fatal("exact descendant still exists")
	}
}
func TestPipeProcessCancellationAndStartFailureNeverLeaveAChild(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cmd := exec.Command("/bin/sleep", "30")
	if _, err := startPipeProcess(ctx, cmd); err == nil || cmd.Process != nil {
		t.Fatal("started after cancellation")
	}
	if _, err := startPipeProcess(context.Background(), exec.Command("/nonexistent/xhs-browser")); err == nil {
		t.Fatal("accepted failed start")
	}
	ctx, cancel = context.WithCancel(context.Background())
	defer cancel()
	cmd = exec.Command(os.Args[0], "-test.run=^TestPipeProcessHelper$")
	cmd.Env = []string{"XHS_TEST_PIPE_HELPER=1", "PATH=/usr/bin:/bin"}
	process, err := startPipeProcess(ctx, cmd)
	if err != nil {
		t.Fatal(err)
	}
	defer process.Close()
	cancel()
	select {
	case <-process.done:
	case <-time.After(3 * time.Second):
		t.Fatal("cancellation did not reap child")
	}
	if syscall.Kill(process.pid, 0) == nil {
		t.Fatal("cancelled child survived")
	}
}

func TestPipeProcessHonorsBoundedOwnerTimeout(t *testing.T) {
	for _, budget := range []time.Duration{0, -time.Second, 5 * time.Minute} {
		cmd := exec.Command("/bin/sleep", "30")
		if _, err := startPipeProcessWithTimeout(context.Background(), cmd, budget); err == nil || cmd.Process != nil {
			t.Fatal("invalid owner timeout started a process")
		}
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestPipeProcessHelper$")
	cmd.Env = []string{"XHS_TEST_PIPE_HELPER=1", "PATH=/usr/bin:/bin"}
	process, err := startPipeProcessWithTimeout(context.Background(), cmd, 500*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	defer process.Close()
	if err := process.transport.Send([]byte(`{"id":1}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := process.transport.Read(); err != nil {
		t.Fatal("helper did not start before its deadline", err)
	}
	select {
	case <-process.done:
	case <-time.After(3 * time.Second):
		t.Fatal("owner timeout did not reap its child")
	}
	if syscall.Kill(process.pid, 0) == nil {
		t.Fatal("expired child still exists")
	}
}
