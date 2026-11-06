package main

import (
	"context"
	"net/url"
	"strings"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/proto"
)

const selfAccountOrigin = "https://www.xiaohongshu.com"
const selfAccountSelector = "div.main-container li.user.side-bar-component a.link-wrapper"

type selfAccountSnapshot struct {
	URL         string `json:"url"`
	Href        string `json:"href"`
	UserID      string `json:"userId"`
	Guest       bool   `json:"guest"`
	Conflicting bool   `json:"conflicting"`
}
type selfAccountPage interface {
	explore(context.Context) error
	snapshot(context.Context) (selfAccountSnapshot, error)
	clickSelf(context.Context) error
}

func selfOriginURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host != "www.xiaohongshu.com" || u.User != nil || u.Opaque != "" || u.RawPath != "" {
		return nil, errAccountMismatch
	}
	return u, nil
}
func selfProfileID(raw string) (string, error) {
	u, err := selfOriginURL(raw)
	if err != nil || !strings.HasPrefix(u.Path, "/user/profile/") {
		return "", errAccountMismatch
	}
	id := strings.TrimPrefix(u.Path, "/user/profile/")
	if !journalID.MatchString(id) {
		return "", errAccountMismatch
	}
	return id, nil
}
func validateSelfSnapshot(s selfAccountSnapshot) (string, error) {
	if _, err := selfOriginURL(s.URL); err != nil {
		return "", errAccountMismatch
	}
	id, err := selfProfileID(s.Href)
	if err != nil || s.Guest || s.Conflicting || s.UserID == "" || s.UserID != id {
		return "", errAccountMismatch
	}
	return id, nil
}

// No nickname, caller-supplied user ID or visited foreign profile can supply self identity.
func readSelfAccount(ctx context.Context, page selfAccountPage) (string, error) {
	if ctx.Err() != nil || page == nil {
		return "", errAccountMismatch
	}
	if err := page.explore(ctx); err != nil {
		return "", errAccountMismatch
	}
	before, err := page.snapshot(ctx)
	if err != nil {
		return "", errAccountMismatch
	}
	id, err := validateSelfSnapshot(before)
	if err != nil {
		return "", errAccountMismatch
	}
	if err = page.clickSelf(ctx); err != nil {
		return "", errAccountMismatch
	}
	after, err := page.snapshot(ctx)
	if err != nil {
		return "", errAccountMismatch
	}
	next, err := validateSelfSnapshot(after)
	if err != nil || next != id {
		return "", errAccountMismatch
	}
	visited, err := selfProfileID(after.URL)
	if err != nil || visited != id || ctx.Err() != nil {
		return "", errAccountMismatch
	}
	return id, nil
}

type rodSelfAccountPage struct{ page *rod.Page }

func (p rodSelfAccountPage) explore(ctx context.Context) error {
	page := p.page.Context(ctx)
	if err := page.Navigate(selfAccountOrigin + "/explore"); err != nil {
		return errAccountMismatch
	}
	if err := page.WaitLoad(); err != nil {
		return errAccountMismatch
	}
	return nil
}

// Read only the bounded identity projection in one evaluation; never serialize
// the full state (which may contain private session/resource tokens).
const selfAccountSnapshotJS = `() => {
 const links=document.querySelectorAll("div.main-container li.user.side-bar-component a.link-wrapper");
 const state=window.__INITIAL_STATE__?.user?.userInfo;
 const info=state && state.value !== undefined ? state.value : (state && state._value !== undefined ? state._value : state);
 const first=info?.userId, second=info?.user_id;
 const id=first !== undefined ? first : second;
 return {
  url: location.href.slice(0,4096),
  href: links.length===1 ? links[0].href.slice(0,4096) : "",
  userId: typeof id==="string" && id.length<=128 ? id : "",
  guest: !info || info.guest===true,
  conflicting: first!==undefined && second!==undefined && first!==second
 };
}`

func (p rodSelfAccountPage) snapshot(ctx context.Context) (selfAccountSnapshot, error) {
	var sample selfAccountSnapshot
	result, err := p.page.Context(ctx).Eval(selfAccountSnapshotJS)
	if err != nil || result == nil {
		return sample, errAccountMismatch
	}
	if err = result.Value.Unmarshal(&sample); err != nil {
		return sample, errAccountMismatch
	}
	return sample, nil
}
func (p rodSelfAccountPage) clickSelf(ctx context.Context) error {
	page := p.page.Context(ctx)
	links, err := page.Elements(selfAccountSelector)
	if err != nil || len(links) != 1 {
		return errAccountMismatch
	}
	href, err := links[0].Attribute("href")
	if err != nil || href == nil {
		return errAccountMismatch
	}
	// Resolve relative sidebar hrefs against the fixed site, never an arbitrary page.
	base, _ := url.Parse(selfAccountOrigin)
	relative, err := url.Parse(*href)
	if err != nil {
		return errAccountMismatch
	}
	if _, err = selfProfileID(base.ResolveReference(relative).String()); err != nil {
		return errAccountMismatch
	}
	if err = links[0].Click(proto.InputMouseButtonLeft, 1); err != nil {
		return errAccountMismatch
	}
	if err = page.WaitLoad(); err != nil {
		return errAccountMismatch
	}
	return nil
}
