package main

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"github.com/xpzouying/xiaohongshu-mcp/browser"
	"golang.org/x/sys/unix"
	"io"
	"net/http"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type startupBinary struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}
type guardedStartup struct {
	SchemaVersion       int           `json:"schemaVersion"`
	ServiceUID          int           `json:"serviceUid"`
	ServiceGID          int           `json:"serviceGid"`
	ModelUID            int           `json:"modelUid"`
	PolicyVersion       int64         `json:"policyVersion"`
	FlywheelRevision    string        `json:"flywheelRevision"`
	ProviderRevision    string        `json:"providerRevision"`
	AccountBase         frozenAccount `json:"accountBase"`
	ProviderBinary      startupBinary `json:"providerBinary"`
	Browser             startupBinary `json:"browser"`
	Guardian            startupBinary `json:"guardian"`
	FFmpeg              startupBinary `json:"ffmpeg"`
	FFprobe             startupBinary `json:"ffprobe"`
	ToolSchemaDigest    string        `json:"toolSchemaDigest"`
	EpochPath           string        `json:"epochPath"`
	JournalPath         string        `json:"journalPath"`
	MediaRoot           string        `json:"mediaRoot"`
	ProfileRoot         string        `json:"profileRoot"`
	ProviderSocket      string        `json:"providerSocket"`
	AuthoritySocket     string        `json:"authoritySocket"`
	KeyPath             string        `json:"keyPath"`
	KeyID               string        `json:"keyId"`
	AcceptancePath      string        `json:"acceptancePath"`
	AcceptancePublicKey string        `json:"acceptancePublicKey"`
}

func guardedPrincipal(model, service, group, uid, gid int, groups []int) bool {
	if model <= 0 || service <= 0 || model == service || group <= 0 || group == 80 || uid != service || gid != group {
		return false
	}
	for _, g := range groups {
		if g != group {
			return false
		}
	}
	return true
}

// No symlink or group/other-writable ancestor is accepted. Immutable policies
// allow only root ancestors; private state additionally allows the service UID.
func guardedAncestors(path string, service int) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return errPrivateProvider
	}
	for current := path; ; current = filepath.Dir(current) {
		info, err := os.Lstat(current)
		if err != nil || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0022 != 0 {
			return errPrivateProvider
		}
		owner, ok := info.Sys().(*syscall.Stat_t)
		if !ok || (owner.Uid != 0 && int(owner.Uid) != service) {
			return errPrivateProvider
		}
		if current != path && !info.IsDir() {
			return errPrivateProvider
		}
		if filepath.Dir(current) == current {
			break
		}
	}
	return nil
}
func readGuardedFile(path string, owner int, mode os.FileMode, limit int64, ancestorService int) ([]byte, error) {
	if guardedAncestors(path, ancestorService) != nil {
		return nil, errPrivateProvider
	}
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, errPrivateProvider
	}
	file := os.NewFile(uintptr(fd), "guarded-startup-file")
	defer file.Close()
	before, err := file.Stat()
	if err != nil || !before.Mode().IsRegular() || before.Mode().Perm() != mode || before.Size() < 1 || before.Size() > limit {
		return nil, errPrivateProvider
	}
	stat, ok := before.Sys().(*syscall.Stat_t)
	if !ok || int(stat.Uid) != owner || stat.Nlink != 1 {
		return nil, errPrivateProvider
	}
	raw, err := io.ReadAll(io.LimitReader(file, limit+1))
	after, endErr := file.Stat()
	current, pathErr := os.Lstat(path)
	if err != nil || endErr != nil || pathErr != nil || int64(len(raw)) != before.Size() || !os.SameFile(before, current) || !before.ModTime().Equal(after.ModTime()) || after.Size() != before.Size() {
		return nil, errPrivateProvider
	}
	return raw, nil
}
func loadGuardedStartup(ctx context.Context, path string) (guardedStartup, []byte, error) {
	var c guardedStartup
	raw, err := readGuardedFile(path, 0, 0644, 16384, 0)
	if err != nil || !strictAuthorityJSON(raw, &c) {
		return c, nil, errPrivateProvider
	}
	groups, err := os.Getgroups()
	if err != nil || !guardedPrincipal(c.ModelUID, c.ServiceUID, c.ServiceGID, os.Geteuid(), os.Getegid(), groups) || c.SchemaVersion != 1 || c.PolicyVersion < 1 || c.PolicyVersion > maxPermitInteger {
		return c, nil, errPrivateProvider
	}
	model, err := user.LookupId(strconv.Itoa(c.ModelUID))
	if err != nil {
		return c, nil, errPrivateProvider
	}
	modelGroups, err := model.GroupIds()
	if err != nil || !guardedSeparateGroup(c.ServiceGID, modelGroups) {
		return c, nil, errPrivateProvider
	}
	revision := regexp.MustCompile(`^[a-f0-9]{40}$`)
	if !revision.MatchString(c.FlywheelRevision) || !revision.MatchString(c.ProviderRevision) || !journalDigest.MatchString(c.ToolSchemaDigest) || !frozenID(c.KeyID) {
		return c, nil, errPrivateProvider
	}
	executable, err := os.Executable()
	if err != nil || executable != c.ProviderBinary.Path {
		return c, nil, errPrivateProvider
	}
	if verifyStartupBinaries(c) != nil {
		return c, nil, errPrivateProvider
	}
	schema, err := measuredGuardedSchema(ctx)
	if err != nil || schema != c.ToolSchemaDigest {
		return c, nil, errPrivateProvider
	}
	configHash := sha256.Sum256(raw)
	publicKey, err := base64.StdEncoding.Strict().DecodeString(c.AcceptancePublicKey)
	if err != nil {
		return c, nil, errPrivateProvider
	}
	proof, err := readGuardedFile(c.AcceptancePath, 0, 0644, 4096, c.ServiceUID)
	if err != nil || verifyGuardedBoundary(proof, publicKey, hex.EncodeToString(configHash[:]), c.ProviderBinary.SHA256, schema) != nil {
		return c, nil, errPrivateProvider
	}
	roots := []string{c.EpochPath, c.JournalPath, c.MediaRoot, c.ProfileRoot}
	for i, root := range roots {
		info, err := os.Lstat(root)
		if err != nil || !privateJournalEntry(info, true) || guardedAncestors(root, c.ServiceUID) != nil {
			return c, nil, errPrivateProvider
		}
		for j, other := range roots {
			if i != j && (root == other || strings.HasPrefix(root, other+string(os.PathSeparator))) {
				return c, nil, errPrivateProvider
			}
		}
	}
	for _, socket := range []string{c.ProviderSocket, c.AuthoritySocket} {
		if !filepath.IsAbs(socket) || filepath.Clean(socket) != socket || guardedAncestors(filepath.Dir(socket), c.ServiceUID) != nil {
			return c, nil, errPrivateProvider
		}
	}
	if _, _, err := authoritySocketSnapshot(c.AuthoritySocket, uint32(c.ServiceGID), os.Lstat); err != nil {
		return c, nil, errPrivateProvider
	}
	// Scratch cleanup may remove only lease-owned media/profile directories.
	for _, protected := range []string{path, c.KeyPath, c.AcceptancePath, c.ProviderSocket, c.AuthoritySocket} {
		for _, root := range []string{c.MediaRoot, c.ProfileRoot} {
			if protected == root || strings.HasPrefix(protected, root+string(os.PathSeparator)) {
				return c, nil, errPrivateProvider
			}
		}
	}
	key, err := readGuardedFile(c.KeyPath, c.ServiceUID, 0600, 32, c.ServiceUID)
	if err != nil || len(key) != 32 {
		return c, nil, errPrivateProvider
	}
	return c, key, nil
}
func runGuardedProvider(ctx context.Context, path string) error {
	c, key, err := loadGuardedStartup(ctx, path)
	if err != nil {
		return errPrivateProvider
	}
	// Only completed receipts bound to this root/account/generation can remove
	// old profiles. Unknown or interrupted owners still fail closed; no PID scan.
	scope := browser.GuardianProfileScope{ProviderInstanceID: c.AccountBase.ProviderInstanceID, AccountUserID: c.AccountBase.AccountUserID, ProviderGeneration: c.AccountBase.ProviderGeneration}
	if browser.ReconcileGuardianProfiles(c.ProfileRoot, scope) != nil {
		return errPrivateProvider
	}
	epochs, err := openAccountEpochStore(c.EpochPath, c.AccountBase, false)
	if err != nil {
		return errPrivateProvider
	}
	journal, err := openWriteJournal(c.JournalPath, c.AccountBase.ProviderGeneration, false)
	if err != nil {
		return errPrivateProvider
	}
	authority, err := newAuthorityClient(c.AuthoritySocket, uint32(c.ServiceGID))
	if err != nil {
		return errPrivateProvider
	}
	defer authority.Close()
	decode, err := newProviderMediaDecoder(mediaBinaryPin{c.FFmpeg.Path, c.FFmpeg.SHA256}, mediaBinaryPin{c.FFprobe.Path, c.FFprobe.SHA256}, 60*time.Second)
	if err != nil {
		return errPrivateProvider
	}
	service, err := newGuardedService(ctx, guardedServiceConfig{Epochs: epochs, Journal: journal, MediaRoot: c.MediaRoot, Browser: browser.PipeBrowserOptions{BinaryPath: c.Browser.Path, BinarySHA256: c.Browser.SHA256, ProfileRoot: c.ProfileRoot}, Upstream: frozenUpstream{BinarySHA256: c.ProviderBinary.SHA256, ToolSchemaDigest: c.ToolSchemaDigest, GuardProtocol: 1}, Execution: providerExecutionPolicy{Audience: c.AccountBase.ProviderInstanceID, KeyID: c.KeyID, Key: key}, Decode: decode, Admit: authority.admit, Resolve: authority.resolve})
	if err != nil {
		return errPrivateProvider
	}
	defer service.Close()
	server, err := newPrivateProviderHTTP(c.ProviderSocket, uint32(c.ServiceUID), guardedRoutes(service, uint32(c.ServiceUID)))
	if err != nil {
		return errPrivateProvider
	}
	defer server.Close()
	done := make(chan error, 1)
	go func() { done <- server.Serve() }()
	select {
	case err = <-done:
		if err != nil && err != http.ErrServerClosed {
			return errPrivateProvider
		}
	case <-ctx.Done():
		if server.Close() != nil {
			return errPrivateProvider
		}
		<-done
	}
	if service.Close() != nil {
		return errPrivateProvider
	}
	return nil
}

func guardedSeparateGroup(serviceGroup int, modelGroups []string) bool {
	if len(modelGroups) == 0 {
		return false
	}
	for _, group := range modelGroups {
		value, err := strconv.Atoi(group)
		if err != nil || value < 0 || value == serviceGroup {
			return false
		}
	}
	return true
}

func verifyStartupBinaries(c guardedStartup) error {
	for _, pin := range []startupBinary{c.ProviderBinary, c.Browser, c.Guardian, c.FFmpeg, c.FFprobe} {
		if browser.VerifyPinnedBinary(pin.Path, pin.SHA256) != nil {
			return errPrivateProvider
		}
	}
	return nil
}
