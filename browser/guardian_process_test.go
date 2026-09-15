package browser

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestGuardianProcessOwnsCDPAndRequiresExactCleanupReceipt(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "guardian")
	if raw, err := exec.Command("cc", "-std=c11", "-Wall", "-Wextra", "-Werror", "../guardian/xhs-browser-guardian.c", "-o", binary).CombinedOutput(); err != nil {
		t.Fatalf("compile %v %s", err, raw)
	}
	for _, mode := range []string{"close", "cancel", "bad-receipt", "connect-failure", "start-failure"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			os.Chmod(root, 0700)
			profile, err := createGuardianProfile(root, guardianScopeFixture)
			if err != nil {
				t.Fatal(err)
			}
			defer profile.receipt.Close()
			cmd := exec.Command(os.Args[0], "-test.run=^TestPipeProcessHelper$")
			cmd.Env = []string{"XHS_TEST_PIPE_HELPER=1", "PATH=/usr/bin:/bin"}
			cmd.Dir = profile.path
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if mode == "connect-failure" || mode == "start-failure" {
				cmd.Env = append(cmd.Env, "XHS_TEST_PIPE_REJECT=1")
				guardian := binary
				if mode == "start-failure" {
					guardian = "/nonexistent/guardian"
				}
				owner, err := launchPreparedGuardianBrowser(ctx, cmd, guardian, profile, 5*time.Second)
				if owner != nil {
					defer owner.Close()
				}
				if err == nil || owner != nil {
					t.Fatal("failed startup cleanup not confirmed", err)
				}
				if entries, _ := os.ReadDir(root); len(entries) != 0 {
					t.Fatal("failed launch retained confirmed-unused profile")
				}
				return
			}
			process, err := startGuardianProcess(ctx, cmd, binary, profile, 5*time.Second)
			if process != nil {
				defer process.Close()
			}
			if err != nil {
				t.Fatal(err)
			}
			if group, err := syscall.Getpgid(process.command.Process.Pid); err != nil || group != process.command.Process.Pid {
				t.Fatal("guardian shares provider process group", group, err)
			}
			if process.transport.Send([]byte(`{"id":1}`)) != nil {
				t.Fatal("send")
			}
			raw, err := process.transport.Read()
			if err != nil {
				t.Fatal(err)
			}
			var result struct{ Result struct{ PID int } }
			if json.Unmarshal(raw, &result) != nil || result.Result.PID != process.pid {
				t.Fatal("CDP child identity")
			}
			if mode == "bad-receipt" {
				if os.Remove(filepath.Join(profile.path, "cleanup.json")) != nil {
					t.Fatal("unlink")
				}
			}
			if mode == "cancel" {
				cancel()
			}
			err = process.Close()
			if mode == "bad-receipt" {
				if err == nil {
					t.Fatal("missing receipt confirmed cleanup")
				}
				if _, err := os.Stat(profile.path); err != nil {
					t.Fatal("lost profile")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := profile.removeCompleted(); err != nil {
				t.Fatal(err)
			}
			if entries, _ := os.ReadDir(root); len(entries) != 0 {
				t.Fatal("completed profile remains")
			}
		})
	}
}
