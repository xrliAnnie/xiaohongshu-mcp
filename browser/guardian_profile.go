package browser

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"unicode/utf8"

	"golang.org/x/sys/unix"
)

type GuardianProfileScope struct {
	ProviderInstanceID string `json:"providerInstanceId"`
	AccountUserID      string `json:"accountUserId"`
	ProviderGeneration string `json:"providerGeneration"`
}
type guardianProfileIdentity struct {
	SchemaVersion int                  `json:"schemaVersion"`
	UID           uint32               `json:"uid"`
	Scope         GuardianProfileScope `json:"scope"`
	Nonce         string               `json:"nonce"`
	RootDevice    uint64               `json:"rootDevice"`
	RootInode     uint64               `json:"rootInode"`
	ProfileDevice uint64               `json:"profileDevice"`
	ProfileInode  uint64               `json:"profileInode"`
}
type guardianCleanupReceipt struct {
	SchemaVersion int    `json:"schemaVersion"`
	Nonce         string `json:"nonce"`
	PID           int    `json:"pid"`
	Cleaned       bool   `json:"cleaned"`
}
type guardianProfile struct {
	root, path string
	identity   guardianProfileIdentity
	receipt    *os.File
}

func validGuardianScope(s GuardianProfileScope) bool {
	for _, value := range []string{s.ProviderInstanceID, s.AccountUserID, s.ProviderGeneration} {
		if len(value) == 0 || len(value) > 256 || !utf8.ValidString(value) || strings.ContainsAny(value, "\x00\r\n") {
			return false
		}
	}
	return true
}
func guardianPrivateDirectory(path string) (os.FileInfo, *syscall.Stat_t, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, nil, errCDPPipe
	}
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode()&(os.ModeSymlink|os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 || info.Mode().Perm() != 0700 {
		return nil, nil, errCDPPipe
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Geteuid()) || stat.Uid == 0 {
		return nil, nil, errCDPPipe
	}
	return info, stat, nil
}
func syncGuardianDirectory(path string) error {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return errCDPPipe
	}
	file := os.NewFile(uintptr(fd), "guardian-directory")
	defer file.Close()
	if file.Sync() != nil {
		return errCDPPipe
	}
	return nil
}
func guardianExclusiveFile(path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
	if err != nil {
		return nil, errCDPPipe
	}
	return os.NewFile(uintptr(fd), "guardian-private-file"), nil
}
func createGuardianProfile(root string, scope GuardianProfileScope) (*guardianProfile, error) {
	if !validGuardianScope(scope) {
		return nil, errCDPPipe
	}
	rootInfo, rootStat, err := guardianPrivateDirectory(root)
	if err != nil {
		return nil, errCDPPipe
	}
	profile, err := os.MkdirTemp(root, "lease-")
	if err != nil {
		return nil, errCDPPipe
	}
	_, profileStat, err := guardianPrivateDirectory(profile)
	if err != nil {
		return nil, errCDPPipe
	}
	nonce := make([]byte, 32)
	if _, err := rand.Read(nonce); err != nil {
		return nil, errCDPPipe
	}
	p := &guardianProfile{root: root, path: profile, identity: guardianProfileIdentity{1, uint32(os.Geteuid()), scope, hex.EncodeToString(nonce), uint64(rootStat.Dev), rootStat.Ino, uint64(profileStat.Dev), profileStat.Ino}}
	raw, err := json.Marshal(p.identity)
	if err != nil {
		return nil, errCDPPipe
	}
	owner, err := guardianExclusiveFile(filepath.Join(profile, "owner.json"))
	if err != nil {
		return nil, errCDPPipe
	}
	_, writeErr := owner.Write(append(raw, '\n'))
	syncErr := owner.Sync()
	closeErr := owner.Close()
	if writeErr != nil || syncErr != nil || closeErr != nil {
		return nil, errCDPPipe
	}
	receipt, err := guardianExclusiveFile(filepath.Join(profile, "cleanup.json"))
	if err != nil {
		return nil, errCDPPipe
	}
	if receipt.Sync() != nil || syncGuardianDirectory(profile) != nil || syncGuardianDirectory(root) != nil {
		receipt.Close()
		return nil, errCDPPipe
	}
	current, _, err := guardianPrivateDirectory(root)
	if err != nil || !os.SameFile(rootInfo, current) {
		receipt.Close()
		return nil, errCDPPipe
	}
	p.receipt = receipt
	return p, nil
}
func readGuardianRecord(path string, result any) error {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return errCDPPipe
	}
	file := os.NewFile(uintptr(fd), "guardian-record")
	defer file.Close()
	before, err := file.Stat()
	if err != nil || !before.Mode().IsRegular() || before.Mode().Perm() != 0600 || before.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 || before.Size() < 1 || before.Size() > 4096 {
		return errCDPPipe
	}
	stat, ok := before.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Geteuid()) || stat.Nlink != 1 {
		return errCDPPipe
	}
	raw, err := io.ReadAll(io.LimitReader(file, 4097))
	if err != nil {
		return errCDPPipe
	}
	after, err := file.Stat()
	current, pathErr := os.Lstat(path)
	if err != nil || pathErr != nil || !os.SameFile(before, current) || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) || int64(len(raw)) != before.Size() {
		return errCDPPipe
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(result) != nil {
		return errCDPPipe
	}
	canonical, err := json.Marshal(result)
	// Our writer has exactly one canonical representation. This also rejects
	// duplicate fields, trailing JSON, omitted fields and alternative encodings.
	if err != nil || !bytes.Equal(raw, append(canonical, '\n')) {
		return errCDPPipe
	}
	return nil
}
func loadGuardianProfileOwner(root, name string, scope GuardianProfileScope) (*guardianProfile, error) {
	if !validGuardianScope(scope) || !strings.HasPrefix(name, "lease-") || len(name) <= 6 || filepath.Base(name) != name {
		return nil, errCDPPipe
	}
	_, rootStat, err := guardianPrivateDirectory(root)
	if err != nil {
		return nil, errCDPPipe
	}
	path := filepath.Join(root, name)
	_, profileStat, err := guardianPrivateDirectory(path)
	if err != nil {
		return nil, errCDPPipe
	}
	var identity guardianProfileIdentity
	if readGuardianRecord(filepath.Join(path, "owner.json"), &identity) != nil {
		return nil, errCDPPipe
	}
	nonce, err := hex.DecodeString(identity.Nonce)
	if err != nil || len(nonce) != 32 || hex.EncodeToString(nonce) != identity.Nonce || identity.SchemaVersion != 1 || identity.UID != uint32(os.Geteuid()) || identity.Scope != scope || identity.RootDevice != uint64(rootStat.Dev) || identity.RootInode != rootStat.Ino || identity.ProfileDevice != uint64(profileStat.Dev) || identity.ProfileInode != profileStat.Ino {
		return nil, errCDPPipe
	}
	return &guardianProfile{root: root, path: path, identity: identity}, nil
}
func loadCompletedGuardianProfile(root, name string, scope GuardianProfileScope) (*guardianProfile, error) {
	owner, err := loadGuardianProfileOwner(root, name, scope)
	if err != nil {
		return nil, errCDPPipe
	}
	path, identity := owner.path, owner.identity
	var receipt guardianCleanupReceipt
	if readGuardianRecord(filepath.Join(path, "cleanup.json"), &receipt) != nil || receipt.SchemaVersion != 1 || receipt.Nonce != identity.Nonce || receipt.PID <= 0 || receipt.PID > 2147483647 || !receipt.Cleaned {
		return nil, errCDPPipe
	}
	return &guardianProfile{root: root, path: path, identity: identity}, nil
}
func (p *guardianProfile) removeCompleted() error {
	current, err := loadCompletedGuardianProfile(p.root, filepath.Base(p.path), p.identity.Scope)
	if err != nil || current.identity != p.identity {
		return errCDPPipe
	}
	// The model cannot write this private root. RemoveAll unlinks symlinks and
	// never follows a browser-created link into a different profile or directory.
	if os.RemoveAll(p.path) != nil || syncGuardianDirectory(p.root) != nil {
		return errCDPPipe
	}
	return nil
}

// ReconcileGuardianProfiles never looks at PIDs or global browser processes.
// All entries must be accounted for before any profile can be removed.
func ReconcileGuardianProfiles(root string, scope GuardianProfileScope) error {
	if !validGuardianScope(scope) {
		return errCDPPipe
	}
	if _, _, err := guardianPrivateDirectory(root); err != nil {
		return errCDPPipe
	}
	fd, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return errCDPPipe
	}
	directory := os.NewFile(uintptr(fd), "guardian-profile-root")
	entries, err := directory.ReadDir(65)
	closeErr := directory.Close()
	if (err != nil && err != io.EOF) || closeErr != nil || len(entries) > 64 {
		return errCDPPipe
	}
	completed := make([]*guardianProfile, 0, len(entries))
	for _, entry := range entries {
		p, err := loadCompletedGuardianProfile(root, entry.Name(), scope)
		if err != nil {
			return errCDPPipe
		}
		completed = append(completed, p)
	}
	for _, p := range completed {
		if p.removeCompleted() != nil {
			return errCDPPipe
		}
	}
	return nil
}

// Only the caller that observed exec.Cmd.Start fail may use this path. No child
// inherited the receipt, so its still-empty exclusive file is not a recovery proof.
func (p *guardianProfile) removeUnstarted() error {
	owner, err := loadGuardianProfileOwner(p.root, filepath.Base(p.path), p.identity.Scope)
	if err != nil || owner.identity != p.identity || p.receipt == nil {
		return errCDPPipe
	}
	info, err := p.receipt.Stat()
	current, pathErr := os.Lstat(filepath.Join(p.path, "cleanup.json"))
	if err != nil || pathErr != nil || info.Size() != 0 || !os.SameFile(info, current) {
		return errCDPPipe
	}
	if p.receipt.Close() != nil {
		return errCDPPipe
	}
	if os.RemoveAll(p.path) != nil || syncGuardianDirectory(p.root) != nil {
		return errCDPPipe
	}
	return nil
}
