package browser

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestGuardianCleansExactGroupOnProviderEOFOrDeadline(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "guardian")
	if out, err := exec.Command("cc", "-std=c11", "-Wall", "-Wextra", "-Werror", "-O2", "../guardian/xhs-browser-guardian.c", "-o", binary).CombinedOutput(); err != nil {
		t.Fatalf("compile guardian: %v %s", err, out)
	}
	for _, mode := range []string{"eof", "deadline", "exited-leader", "invalid-receipt"} {
		t.Run(mode, func(t *testing.T) {
			pipe := func() (*os.File, *os.File) {
				r, w, err := os.Pipe()
				if err != nil {
					t.Fatal(err)
				}
				return r, w
			}
			commandR, commandW := pipe()
			responseR, responseW := pipe()
			liveR, liveW := pipe()
			statusR, statusW := pipe()
			for _, file := range []*os.File{commandR, commandW, responseR, responseW, liveR, liveW, statusR, statusW} {
				defer file.Close()
			}
			receiptPath := filepath.Join(t.TempDir(), "cleanup.json")
			receipt, err := os.OpenFile(receiptPath, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
			if err != nil {
				t.Fatal(err)
			}
			defer receipt.Close()
			budget := "5000"
			if mode == "invalid-receipt" {
				if _, err := receipt.WriteString("old-receipt"); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "deadline" {
				budget = "800"
			}
			nonce := strings.Repeat("a", 64)
			cmd := exec.Command(binary, budget, nonce, os.Args[0], "-test.run=^TestPipeProcessHelper$")
			cmd.Env = []string{"XHS_TEST_PIPE_HELPER=1", "XHS_TEST_PIPE_DESCENDANT=1", "PATH=/usr/bin:/bin"}
			if mode == "exited-leader" {
				cmd.Env = append(cmd.Env, "XHS_TEST_PIPE_EXIT=1")
			}
			cmd.ExtraFiles = []*os.File{commandR, responseW, liveR, receipt, statusW}
			if err = cmd.Start(); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { done <- cmd.Wait() }()
			waited := false
			defer func() {
				liveW.Close()
				if !waited {
					<-done
				}
			}()
			commandR.Close()
			responseW.Close()
			liveR.Close()
			statusW.Close()
			statusR.SetReadDeadline(time.Now().Add(3 * time.Second))
			line, err := bufio.NewReader(statusR).ReadString('\n')
			if mode == "invalid-receipt" {
				if err == nil || line != "" {
					t.Fatal("started with reused receipt")
				}
				result := <-done
				waited = true
				if result == nil {
					t.Fatal("invalid receipt accepted")
				}
				raw, readErr := os.ReadFile(receiptPath)
				if readErr != nil || string(raw) != "old-receipt" {
					t.Fatal("reused receipt changed")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			var pid int
			if _, err := fmt.Sscanf(line, "START %d\n", &pid); err != nil || pid <= 0 {
				t.Fatal("invalid start", line)
			}
			peer := newPipeTransport(responseR, commandW)
			defer peer.Close()
			responseR.SetReadDeadline(time.Now().Add(3 * time.Second))
			if err := peer.Send([]byte(`{"id":1}`)); err != nil {
				t.Fatal(err)
			}
			raw, err := peer.Read()
			if err != nil {
				t.Fatal(err)
			}
			var observed struct{ Result struct{ PID, Child int } }
			if json.Unmarshal(raw, &observed) != nil || observed.Result.PID != pid || observed.Result.Child <= 0 {
				t.Fatal("lineage absent", string(raw))
			}
			unrelated := exec.Command("/bin/sleep", "30")
			if err := unrelated.Start(); err != nil {
				t.Fatal(err)
			}
			defer func() { _ = unrelated.Process.Kill(); _ = unrelated.Wait() }()
			if mode == "exited-leader" {
				// CDP EOF proves the leader closed its descriptors. Its PID must
				// remain reserved by the guardian until the exact-group kill.
				if _, err := peer.Read(); err == nil {
					t.Fatal("leader did not exit")
				}
				if syscall.Kill(pid, 0) != nil {
					t.Fatal("guardian reaped leader too early")
				}
			}
			if mode == "eof" || mode == "exited-leader" {
				liveW.Close()
			}
			select {
			case err := <-done:
				waited = true
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(4 * time.Second):
				t.Fatal("guardian did not exit")
			}
			var proof struct {
				SchemaVersion int    `json:"schemaVersion"`
				Nonce         string `json:"nonce"`
				PID           int    `json:"pid"`
				Cleaned       bool   `json:"cleaned"`
			}
			raw, err = os.ReadFile(receiptPath)
			if err != nil || json.Unmarshal(raw, &proof) != nil || proof.SchemaVersion != 1 || proof.Nonce != nonce || proof.PID != pid || !proof.Cleaned {
				t.Fatal("cleanup receipt", string(raw), err)
			}
			deadline := time.Now().Add(2 * time.Second)
			for syscall.Kill(observed.Result.Child, 0) == nil && time.Now().Before(deadline) {
				time.Sleep(10 * time.Millisecond)
			}
			if syscall.Kill(pid, 0) == nil || syscall.Kill(observed.Result.Child, 0) == nil {
				t.Fatal("owned lineage remains")
			}
			if unrelated.Process.Signal(syscall.Signal(0)) != nil {
				t.Fatal("unrelated child was killed")
			}
		})
	}
}
