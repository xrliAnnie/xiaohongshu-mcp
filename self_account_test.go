package main

import (
	"context"
	"testing"
)

type selfPageFixture struct {
	samples                    []selfAccountSnapshot
	reads, clicks, navigations int
}

func (p *selfPageFixture) explore(context.Context) error { p.navigations++; return nil }
func (p *selfPageFixture) snapshot(context.Context) (selfAccountSnapshot, error) {
	i := p.reads
	p.reads++
	return p.samples[i], nil
}
func (p *selfPageFixture) clickSelf(context.Context) error { p.clicks++; return nil }
func goodSelfSnapshot(profile bool) selfAccountSnapshot {
	s := selfAccountSnapshot{URL: "https://www.xiaohongshu.com/explore", Href: "https://www.xiaohongshu.com/user/profile/account-a", UserID: "account-a"}
	if profile {
		s.URL = s.Href
	}
	return s
}
func TestSelfAccountRequiresSidebarAndStateOnSamePage(t *testing.T) {
	p := &selfPageFixture{samples: []selfAccountSnapshot{goodSelfSnapshot(false), goodSelfSnapshot(true)}}
	id, err := readSelfAccount(context.Background(), p)
	if err != nil || id != "account-a" || p.navigations != 1 || p.clicks != 1 || p.reads != 2 {
		t.Fatal("missing same page proof", id, err)
	}
}
func TestSelfAccountRejectsMissingConflictingGuestOrForeignProof(t *testing.T) {
	for _, test := range []struct {
		name string
		edit func(*selfAccountSnapshot)
	}{
		{"missing state", func(s *selfAccountSnapshot) { s.UserID = "" }},
		{"other account", func(s *selfAccountSnapshot) { s.UserID = "account-b" }},
		{"guest", func(s *selfAccountSnapshot) { s.Guest = true }},
		{"conflicting aliases", func(s *selfAccountSnapshot) { s.Conflicting = true }},
		{"foreign page", func(s *selfAccountSnapshot) { s.URL = "https://evil.example/explore" }},
		{"foreign self", func(s *selfAccountSnapshot) { s.Href = "https://evil.example/user/profile/account-a" }},
		{"credential URL", func(s *selfAccountSnapshot) { s.Href = "https://user@www.xiaohongshu.com/user/profile/account-a" }},
		{"encoded ID", func(s *selfAccountSnapshot) { s.Href = "https://www.xiaohongshu.com/user/profile/%61ccount-a" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			s := goodSelfSnapshot(false)
			test.edit(&s)
			p := &selfPageFixture{samples: []selfAccountSnapshot{s}}
			if _, err := readSelfAccount(context.Background(), p); err == nil {
				t.Fatal("accepted invalid identity")
			}
			if p.clicks != 0 {
				t.Fatal("navigated invalid self link")
			}
		})
	}
}
func TestSelfAccountRejectsSwitchAfterSidebarNavigation(t *testing.T) {
	for _, test := range []struct {
		name string
		edit func(*selfAccountSnapshot)
	}{
		{"state changed", func(s *selfAccountSnapshot) { s.UserID = "account-b" }},
		{"other profile", func(s *selfAccountSnapshot) { s.URL = "https://www.xiaohongshu.com/user/profile/account-b" }},
		{"login redirect", func(s *selfAccountSnapshot) { s.URL = "https://www.xiaohongshu.com/login" }},
		{"new sidebar", func(s *selfAccountSnapshot) { s.Href = "https://www.xiaohongshu.com/user/profile/account-b" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			after := goodSelfSnapshot(true)
			test.edit(&after)
			p := &selfPageFixture{samples: []selfAccountSnapshot{goodSelfSnapshot(false), after}}
			if _, err := readSelfAccount(context.Background(), p); err == nil {
				t.Fatal("accepted switched account")
			}
		})
	}
}
