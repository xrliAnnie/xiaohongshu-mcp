package browser

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

var guardianScopeFixture = GuardianProfileScope{ProviderInstanceID: "provider-a", AccountUserID: "account-a", ProviderGeneration: "generation-a"}

func finishGuardianFixture(t *testing.T, p *guardianProfile) {
	t.Helper()
	raw, err := json.Marshal(guardianCleanupReceipt{SchemaVersion: 1, Nonce: p.identity.Nonce, PID: 123, Cleaned: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = p.receipt.Write(append(raw, '\n')); err != nil {
		t.Fatal(err)
	}
	if p.receipt.Sync() != nil || p.receipt.Close() != nil {
		t.Fatal("close receipt")
	}
}
func TestGuardianProfileRecoversOnlyExactCompletedScope(t *testing.T) {
	root := t.TempDir()
	if os.Chmod(root, 0700) != nil {
		t.Fatal("chmod")
	}
	p, err := createGuardianProfile(root, guardianScopeFixture)
	if err != nil {
		t.Fatal(err)
	}
	defer p.receipt.Close()
	if ReconcileGuardianProfiles(root, guardianScopeFixture) == nil {
		t.Fatal("empty receipt accepted")
	}
	if _, err := os.Stat(p.path); err != nil {
		t.Fatal("incomplete profile removed")
	}
	finishGuardianFixture(t, p)
	wrong := guardianScopeFixture
	wrong.ProviderGeneration = "other"
	if ReconcileGuardianProfiles(root, wrong) == nil {
		t.Fatal("wrong generation accepted")
	}
	if err := os.WriteFile(filepath.Join(p.path, "browser-data"), []byte("synthetic"), 0600); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	target := filepath.Join(outside, "keep")
	if os.WriteFile(target, []byte("keep"), 0600) != nil || os.Symlink(outside, filepath.Join(p.path, "outside-link")) != nil {
		t.Fatal("symlink fixture")
	}
	if err := ReconcileGuardianProfiles(root, guardianScopeFixture); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p.path); !os.IsNotExist(err) {
		t.Fatal("completed profile remains")
	}
	if raw, err := os.ReadFile(target); err != nil || string(raw) != "keep" {
		t.Fatal("followed profile symlink")
	}
}
func TestGuardianProfileRejectsTamperedIdentityAndReceipt(t *testing.T) {
	for _, mode := range []string{"nonce", "uid", "scope", "inode", "duplicate", "malformed", "symlink", "mode", "unknown", "replaced-directory"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			os.Chmod(root, 0700)
			p, err := createGuardianProfile(root, guardianScopeFixture)
			if err != nil {
				t.Fatal(err)
			}
			finishGuardianFixture(t, p)
			owner := p.identity
			switch mode {
			case "nonce":
				owner.Nonce = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
			case "uid":
				owner.UID++
			case "scope":
				owner.Scope.AccountUserID = "other"
			case "inode":
				owner.ProfileInode++
			case "duplicate":
				raw, _ := os.ReadFile(filepath.Join(p.path, "owner.json"))
				raw = append([]byte(`{"schemaVersion":1,`), raw[1:]...)
				os.WriteFile(filepath.Join(p.path, "owner.json"), raw, 0600)
			case "malformed":
				os.WriteFile(filepath.Join(p.path, "cleanup.json"), []byte(`{}`), 0600)
			case "symlink":
				os.Remove(filepath.Join(p.path, "cleanup.json"))
				os.Symlink("owner.json", filepath.Join(p.path, "cleanup.json"))
			case "mode":
				os.Chmod(filepath.Join(p.path, "owner.json"), 0644)
			case "unknown":
				os.Mkdir(filepath.Join(root, "unaccounted"), 0700)
			case "replaced-directory":
				old := p.path + "-old"
				if os.Rename(p.path, old) != nil || os.Mkdir(p.path, 0700) != nil {
					t.Fatal("replace directory")
				}
				for _, name := range []string{"owner.json", "cleanup.json"} {
					raw, err := os.ReadFile(filepath.Join(old, name))
					if err != nil {
						t.Fatal(err)
					}
					if os.WriteFile(filepath.Join(p.path, name), raw, 0600) != nil {
						t.Fatal("copy identity")
					}
				}
			}
			if mode == "nonce" || mode == "uid" || mode == "scope" || mode == "inode" {
				raw, _ := json.Marshal(owner)
				os.WriteFile(filepath.Join(p.path, "owner.json"), append(raw, '\n'), 0600)
			}
			if ReconcileGuardianProfiles(root, guardianScopeFixture) == nil {
				t.Fatal("unsafe recovery accepted")
			}
			if _, err := os.Stat(p.path); err != nil {
				t.Fatal("profile removed on refusal")
			}
		})
	}
}
