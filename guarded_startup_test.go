package main

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGuardedStartupRejectsModelOwnedConfiguration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if os.WriteFile(path, []byte(`{}`), 0644) != nil {
		t.Fatal("fixture")
	}
	if _, err := readGuardedFile(path, 0, 0644, 16384, 0); err == nil {
		t.Fatal("model-writable configuration accepted")
	}
}
func TestGuardedStartupPrincipalSeparatesServiceAndModel(t *testing.T) {
	if !guardedPrincipal(501, 502, 600, 502, 600, []int{600}) {
		t.Fatal("dedicated principal rejected")
	}
	for _, test := range []struct {
		model, service, group, uid, gid int
		groups                          []int
	}{
		{501, 501, 600, 501, 600, []int{600}}, {501, 0, 600, 0, 600, []int{600}},
		{501, 502, 600, 501, 600, []int{600}}, {501, 502, 600, 502, 600, []int{600, 80}},
	} {
		if guardedPrincipal(test.model, test.service, test.group, test.uid, test.gid, test.groups) {
			t.Fatal("unsafe principal accepted")
		}
	}
}
func TestGuardedStartupRejectsSharedModelGroup(t *testing.T) {
	if !guardedSeparateGroup(600, []string{"20", "80"}) {
		t.Fatal("independent group rejected")
	}
	for _, groups := range [][]string{{"20", "600"}, {"bad"}, {}} {
		if guardedSeparateGroup(600, groups) {
			t.Fatal("shared or unknown group accepted")
		}
	}
}

func TestGuardedStartupPinsGuardianWithOtherExecutables(t *testing.T) {
	raw, err := os.ReadFile("/usr/bin/true")
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	pin := startupBinary{Path: "/usr/bin/true", SHA256: hex.EncodeToString(sum[:])}
	c := guardedStartup{ProviderBinary: pin, Browser: pin, Guardian: pin, FFmpeg: pin, FFprobe: pin}
	if verifyStartupBinaries(c) != nil {
		t.Fatal("root-owned pinned fixture rejected")
	}
	for _, bad := range []startupBinary{{}, {Path: pin.Path, SHA256: strings.Repeat("0", 64)}, {Path: filepath.Join(t.TempDir(), "guardian"), SHA256: pin.SHA256}} {
		c.Guardian = bad
		if verifyStartupBinaries(c) == nil {
			t.Fatal("missing/unpinned guardian accepted")
		}
	}
}
