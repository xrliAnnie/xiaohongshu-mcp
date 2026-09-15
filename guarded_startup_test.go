package main

import (
	"os"
	"path/filepath"
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
