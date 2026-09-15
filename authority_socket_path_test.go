package main

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

type authorityPathInfo struct {
	os.FileInfo
	mode os.FileMode
	stat syscall.Stat_t
}

func (i authorityPathInfo) Mode() os.FileMode { return i.mode }
func (i authorityPathInfo) IsDir() bool       { return i.mode.IsDir() }
func (i authorityPathInfo) Sys() any          { return &i.stat }

func TestAuthoritySocketRootLayout(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "authority.sock")
	seed, err := os.Stat(root)
	if err != nil {
		t.Fatal(err)
	}
	mutate := func(string, *authorityPathInfo) {}
	observe := func(p string) (os.FileInfo, error) {
		stat := *seed.Sys().(*syscall.Stat_t)
		stat.Uid = 0
		stat.Gid = 0
		stat.Nlink = 1
		info := authorityPathInfo{seed, os.ModeDir | 0755, stat}
		if p == root {
			info.mode = os.ModeDir | 0750
			info.stat.Gid = 450
		}
		if p == path {
			info.mode = os.ModeSocket | 0660
			info.stat.Gid = 450
		}
		mutate(p, &info)
		return info, nil
	}
	if _, _, err := authoritySocketSnapshot(path, 450, observe); err != nil {
		t.Fatal(err)
	}
	for _, variant := range []string{"socket-owner", "socket-group", "socket-mode", "socket-link", "symlink", "parent-owner", "parent-mode", "parent-group", "ancestor-owner", "ancestor-write"} {
		t.Run(variant, func(t *testing.T) {
			mutate = func(p string, i *authorityPathInfo) {
				switch {
				case p == path && variant == "socket-owner":
					i.stat.Uid = 450
				case p == path && variant == "socket-group":
					i.stat.Gid = 451
				case p == path && variant == "socket-mode":
					i.mode = os.ModeSocket | 0666
				case p == path && variant == "socket-link":
					i.stat.Nlink = 2
				case p == path && variant == "symlink":
					i.mode = os.ModeSymlink | 0660
				case p == root && variant == "parent-owner":
					i.stat.Uid = 450
				case p == root && variant == "parent-mode":
					i.mode = os.ModeDir | 0700
				case p == root && variant == "parent-group":
					i.stat.Gid = 451
				case p == filepath.Dir(root) && variant == "ancestor-owner":
					i.stat.Uid = 501
				case p == filepath.Dir(root) && variant == "ancestor-write":
					i.mode = os.ModeDir | 0777
				}
			}
			if _, _, err := authoritySocketSnapshot(path, 450, observe); err == nil {
				t.Fatal("unsafe layout accepted")
			}
		})
	}
	mutate = func(string, *authorityPathInfo) {}
	for _, group := range []uint32{0, 80, 451} {
		if _, _, err := authoritySocketSnapshot(path, group, observe); err == nil {
			t.Fatal("wrong group accepted")
		}
	}
	if _, _, err := authoritySocketSnapshot(filepath.Join(root, "non-policy.sock"), 450, observe); err == nil {
		t.Fatal("non-policy socket accepted")
	}
}
