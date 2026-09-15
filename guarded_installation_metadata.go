package main

import "strings"

const guardedInstallationMetadataPath = "/Library/Application Support/Flywheel/Xhs/installation.metadata"

type guardedInstallationMetadata struct {
	ManifestSHA256  string
	BootstrapSHA256 string
}

// The caller authenticates the fixed root-owned file before parsing these pins.
func parseGuardedInstallationMetadata(raw []byte) (guardedInstallationMetadata, error) {
	var result guardedInstallationMetadata
	if len(raw) > 1024 {
		return result, errPrivateProvider
	}
	lines := strings.Split(string(raw), "\n")
	if len(lines) != 4 || lines[0] != "version=1" || lines[3] != "" || !strings.HasPrefix(lines[1], "manifest_sha256=") || !strings.HasPrefix(lines[2], "bootstrap_sha256=") {
		return result, errPrivateProvider
	}
	result.ManifestSHA256 = strings.TrimPrefix(lines[1], "manifest_sha256=")
	result.BootstrapSHA256 = strings.TrimPrefix(lines[2], "bootstrap_sha256=")
	if !journalDigest.MatchString(result.ManifestSHA256) || !journalDigest.MatchString(result.BootstrapSHA256) {
		return guardedInstallationMetadata{}, errPrivateProvider
	}
	return result, nil
}
