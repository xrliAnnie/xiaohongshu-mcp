package main

import (
	"strings"
	"testing"
)

func TestGuardedInstallationMetadataGrammar(t *testing.T) {
	manifest, bootstrap := strings.Repeat("a", 64), strings.Repeat("b", 64)
	raw := "version=1\nmanifest_sha256=" + manifest + "\nbootstrap_sha256=" + bootstrap + "\n"
	got, err := parseGuardedInstallationMetadata([]byte(raw))
	if err != nil || got.ManifestSHA256 != manifest || got.BootstrapSHA256 != bootstrap {
		t.Fatal("valid metadata refused", err)
	}
	for _, bad := range []string{raw + "\n", strings.Replace(raw, "version=1", "version=2", 1), strings.Replace(raw, "bootstrap_sha256", "bootstrap_policy_sha256", 1), strings.Replace(raw, bootstrap, strings.ToUpper(bootstrap), 1), strings.Replace(raw, "\n", "\r\n", 1)} {
		if _, err := parseGuardedInstallationMetadata([]byte(bad)); err == nil {
			t.Fatal("bad metadata accepted")
		}
	}
}
