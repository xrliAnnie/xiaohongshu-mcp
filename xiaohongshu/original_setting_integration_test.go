//go:build integration

package xiaohongshu

import (
	"context"
	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/launcher"
	"github.com/go-rod/rod/lib/proto"
	"os"
	"testing"
	"time"
)

func TestOriginalSettingSyntheticDOM(t *testing.T) {
	binary := os.Getenv("XHS_TEST_BROWSER_BIN")
	if binary == "" {
		t.Fatal("XHS_TEST_BROWSER_BIN required for isolated DOM fixture")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	profile := t.TempDir()
	if os.Chmod(profile, 0700) != nil {
		t.Fatal("profile chmod")
	}
	launch := launcher.New().Context(ctx).Bin(binary).UserDataDir(profile).Headless(true).Leakless(false).Set("disable-background-networking").Set("disable-component-update").Set("disable-sync").Set("disable-extensions").Env("PATH=/usr/bin:/bin", "LANG=C", "LC_ALL=C", "HOME="+profile, "TMPDIR="+profile)
	control, err := launch.Launch()
	if err != nil {
		t.Fatal(err)
	}
	defer launch.Kill()
	b := rod.New().Context(ctx).ControlURL(control).Monitor("").Trace(false)
	if err = b.Connect(); err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	page, err := b.Page(proto.TargetCreateTarget{URL: "about:blank"})
	if err != nil {
		t.Fatal(err)
	}
	defer page.Close()
	html := `<html><body>
 <div class="custom-switch-card">原创声明
 <div class="d-switch" style="width:100px;height:40px" onclick="window.switches++; if(document.getElementById('original').checked){document.getElementById('original').checked=false;}else{document.getElementById('dialog').style.display='block';}">
 <input id="original" type="checkbox" style="pointer-events:none"></div></div>
 <div id="dialog" class="footer" style="display:none">原创声明须知 声明原创
 <div class="d-checkbox"><input type="checkbox"></div>
 <button class="custom-button" onclick="document.getElementById('original').checked=true;document.getElementById('dialog').style.display='none'">声明原创</button></div>
 <script>window.switches=0</script></body></html>`
	if err = page.SetDocumentContent(html); err != nil {
		t.Fatal(err)
	}
	if err = setOriginalState(page, true); err != nil {
		t.Fatal("could not enable", err)
	}
	if err = setOriginalState(page, true); err != nil {
		t.Fatal("could not verify enabled", err)
	}
	if page.MustEval(`()=>window.switches`).Int() != 1 {
		t.Fatal("repeated already desired toggle")
	}
	if err = setOriginalState(page, false); err != nil {
		t.Fatal("could not disable", err)
	}
	if page.MustEval(`()=>document.getElementById('original').checked`).Bool() {
		t.Fatal("false ignored")
	}
	page.MustEval(`()=>document.getElementById('original').remove()`)
	if err = setOriginalState(page, false); err == nil {
		t.Fatal("missing checkbox inferred false")
	}
	if err = page.SetDocumentContent(html); err != nil {
		t.Fatal(err)
	}
	page.MustEval(`()=>document.querySelector('button').disabled=true`)
	if err = setOriginalState(page.Timeout(5*time.Second), true); err == nil {
		t.Fatal("ignored failed confirmation")
	}
}
