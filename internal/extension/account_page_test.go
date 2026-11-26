package extension

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/nwverse/persona/sdk/go/exttest"
	"github.com/nwverse/persona/sdk/go/wire"

	pac "github.com/nwverse/personal-agent-club"
	"github.com/nwverse/personal-agent-club/internal/core"
	"github.com/nwverse/personal-agent-club/internal/protocol"
	"github.com/nwverse/personal-agent-club/internal/provider"
	"github.com/nwverse/personal-agent-club/internal/provider/echo"
)

// renameSection is the index of the account page's section with the name field, -1 when there is none.
func renameSection(v *wire.View) int {
	for i, s := range v.Sections {
		for _, c := range s.Items {
			if c.Type == "field" && c.Name == fieldName {
				return i
			}
		}
	}
	return -1
}

// buttonsIn are the titles of a section's buttons.
func buttonsIn(s wire.Section) []string {
	var out []string
	for _, c := range s.Items {
		if c.Type == "button" {
			out = append(out, c.Title)
		}
	}
	return out
}

func has(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// renderAt is ui.render of the accounts UI at a UI version and in a language.
func renderAt(t *testing.T, h *exttest.Host, version int, locale string, st pageState) *wire.View {
	t.Helper()
	var v wire.View
	if err := h.Call("ui.render", wire.UIParams{Contribution: AccountsUI, Placement: wire.PlacementPluginPage, Size: "full",
		UIVersion: version, Locale: locale, State: stateOf(st)}, &v); err != nil {
		t.Fatalf("ui.render %+v: %v", st, err)
	}
	checkView(t, &v)
	return &v
}

// An account's page is pushed (Push) and the host draws the way back above it; the page draws no back control of its
// own, so there are never two. Apps before UI version 3 draw the pushed page in place (the SDK answers their taps) and
// the page draws none for them either: nothing here tells those apps apart, and nothing here ever drew one.
func TestAccountPageDrawsNoBackControlOfItsOwn(t *testing.T) {
	h, club := start(t)
	act(t, h, pageState{Page: pagePair, Provider: echo.Name}, actPair, map[string]string{fieldCredential: demoCredential})
	space, _ := club.Space(context.Background(), core.DefaultSpace)
	status, _ := space.Status(context.Background())
	id := status.Accounts[0].AccountID
	for _, version := range []int{2, 3, 6, 11} {
		for _, st := range []pageState{{Page: pageAccount, Account: id}, {Page: pagePair, Provider: echo.Name}, {Page: pageDone, Account: id}} {
			for _, locale := range []string{"zh-Hans", "en"} {
				v := renderAt(t, h, version, locale, st)
				for _, s := range v.Sections {
					for _, c := range s.Items {
						back := c.Action != nil && c.Action.ID == "back"
						if back || strings.Contains(c.Title, "返回") || strings.HasPrefix(c.Title, "‹") || strings.EqualFold(c.Title, "Back") {
							t.Errorf("UI %d %s %s: a back control of its own: %+v", version, locale, st.Page, c)
						}
					}
				}
			}
		}
	}
}

// The rename block says 名字 once: the field's label (which the apps require); the block has no title repeating it.
func TestRenameBlockSaysNameOnce(t *testing.T) {
	h, club := start(t)
	act(t, h, pageState{Page: pagePair, Provider: echo.Name}, actPair, map[string]string{fieldCredential: demoCredential})
	space, _ := club.Space(context.Background(), core.DefaultSpace)
	status, _ := space.Status(context.Background())
	for _, c := range []struct{ locale, name string }{{"zh-Hans", "名字"}, {"en", "Name"}} {
		v := renderAt(t, h, 6, c.locale, pageState{Page: pageAccount, Account: status.Accounts[0].AccountID})
		i := renameSection(v)
		if i < 0 {
			t.Fatalf("%s: no rename block", c.locale)
		}
		s := v.Sections[i]
		n := strings.Count(s.Title, c.name)
		for _, it := range s.Items {
			n += strings.Count(it.Label, c.name) + strings.Count(it.Title, c.name)
		}
		if n != 1 || s.Title != "" {
			t.Errorf("%s: %q said %d times: %+v", c.locale, c.name, n, s)
		}
		if f := renameField(t, v); f.Label != c.name {
			t.Errorf("%s: field %+v", c.locale, f)
		}
	}
}

// 重新登录 (Grok Bot only: the one sign-in a page can start again) leads the page only while the account needs it;
// while it works it waits under 同步 with a line saying when it is for. Other accounts never show it.
func TestSignInAgainLeadsOnlyWhenTheAccountNeedsIt(t *testing.T) {
	club := core.New(core.Options{Registry: provider.NewRegistry(&fakeGrok{}, echo.New()), FollowUps: []time.Duration{}, WatchLimit: -1})
	h := startHost(t, pac.Manifest, exttest.Serve(t, Program(club, pac.Version())), json.RawMessage(`{"demo_echo":true}`))
	ctx := context.Background()
	space, _ := club.Space(ctx, core.DefaultSpace)
	grok, err := space.AddAccount(ctx, protocol.AddAccountParams{Provider: grokBot, Credential: grokCredential})
	if err != nil {
		t.Fatal(err)
	}
	demo, err := space.AddAccount(ctx, protocol.AddAccountParams{Provider: echo.Name, Credential: demoCredential})
	if err != nil {
		t.Fatal(err)
	}
	type where struct{ top, syncing bool }
	look := func(locale, id string) (where, string) {
		v := renderAt(t, h, 6, locale, pageState{Page: pageAccount, Account: id})
		again := map[string]string{"zh-Hans": "重新登录", "en": "Sign in again"}[locale]
		w := where{top: has(buttonsIn(v.Sections[0]), again)}
		footer := ""
		if i := sectionIndex(v, map[string]string{"zh-Hans": "同步", "en": "Syncing"}[locale]); i >= 0 {
			w.syncing, footer = has(buttonsIn(v.Sections[i]), again), v.Sections[i].Footer
		}
		return w, footer
	}
	for _, locale := range []string{"zh-Hans", "en"} {
		if w, footer := look(locale, grok.AccountID); w.top || !w.syncing ||
			!strings.Contains(footer, map[string]string{"zh-Hans": "只在 Grok 的登录失效时才需要", "en": "only needed once Grok's sign-in stops working"}[locale]) {
			t.Errorf("%s, working Grok Bot: %+v, footer %q", locale, w, footer)
		}
		if w, _ := look(locale, demo.AccountID); w.top || w.syncing {
			t.Errorf("%s, demo account: %+v", locale, w)
		}
	}
	_ = space.Store().UpdateAccount(ctx, grok.AccountID, storeStatus(protocol.AccountError, "AUTH_FAILED"), 1)
	for _, locale := range []string{"zh-Hans", "en"} {
		if w, footer := look(locale, grok.AccountID); !w.top || w.syncing || strings.Contains(footer, "Grok") {
			t.Errorf("%s, signed-out Grok Bot: %+v, footer %q", locale, w, footer)
		}
	}
	// a hiccup (still active, a last error) needs it too
	_ = space.Store().UpdateAccount(ctx, grok.AccountID, storeStatus(protocol.AccountActive, "AUTH_FAILED"), 1)
	if w, _ := look("zh-Hans", grok.AccountID); !w.top || w.syncing {
		t.Errorf("Grok Bot with a recent problem: %+v", w)
	}
}

// With no allowance reported there is nothing to choose: the block is the one sentence. Once one is reported, the
// choice of which the avatar shows is there.
func TestQuotaBlockWithNothingReportedIsOneSentence(t *testing.T) {
	for _, l := range []lang{"zh-Hans", "en"} {
		s := quotaDisplaySection(l, protocol.Account{}, clock{now: time.Now()})
		if len(s.Items) != 1 || s.Items[0].Type != "text" || s.Footer != "" ||
			s.Items[0].Text != l.t("这个服务还没告诉我们额度用了多少。", "The service has not reported any allowance yet.") {
			t.Errorf("%s, nothing reported: %+v", l, s)
		}
		pct := 62
		s = quotaDisplaySection(l, protocol.Account{ShowQuota: true, Quota: &protocol.Quota{State: protocol.QuotaOK, Percent: &pct,
			Kind: protocol.QuotaKindRequests, Selected: "requests:day",
			Buckets: []protocol.QuotaBucket{{ID: "requests:day", Kind: protocol.QuotaKindRequests, Window: "day", Percent: &pct}}}}, clock{now: time.Now()})
		var choice *wire.Component
		for i, c := range s.Items {
			if c.Type == "segmented" && c.Name == fieldQuotaDisplay {
				choice = &s.Items[i]
			}
		}
		if choice == nil || len(choice.Choices) < 3 || s.Footer == "" {
			t.Errorf("%s, reported: %+v", l, s)
		}
	}
	// on the page: a demo account reports nothing
	h, club := start(t)
	act(t, h, pageState{Page: pagePair, Provider: echo.Name}, actPair, map[string]string{fieldCredential: demoCredential})
	space, _ := club.Space(context.Background(), core.DefaultSpace)
	status, _ := space.Status(context.Background())
	v := render(t, h, pageState{Page: pageAccount, Account: status.Accounts[0].AccountID})
	if strings.Contains(text(v), `"name":"`+fieldQuotaDisplay+`"`) || !strings.Contains(text(v), "这个服务还没告诉我们额度用了多少。") {
		t.Errorf("account page: %s", text(v))
	}
}

// 账号 says in one line, first, what it is for and that the overview is the home, in both languages.
func TestAccountsPageSaysWhatItIsFor(t *testing.T) {
	h, _ := start(t)
	for _, c := range []struct {
		locale string
		want   []string
	}{
		{"zh-Hans", []string{"接入的全部账号", "管理", "添加", "「" + homeTitle + "」", "导航"}},
		{"en", []string{"Every account you connected", "manage", "add another", homeTitle, "navigation"}},
	} {
		v := renderAt(t, h, 6, c.locale, pageState{})
		top := v.Sections[0]
		if top.Title != "" || len(top.Items) != 1 || top.Items[0].Type != "text" {
			t.Fatalf("%s: the first section is not one line: %+v", c.locale, top)
		}
		for _, w := range c.want {
			if !strings.Contains(top.Items[0].Text, w) {
				t.Errorf("%s: %q lacks %q", c.locale, top.Items[0].Text, w)
			}
		}
	}
	// the home does not carry it
	if home := renderUI(t, h, HomeUI, wire.PlacementNavPage, pageState{}); strings.Contains(text(home), accountsAbout("zh-Hans")) {
		t.Error("the home carries the accounts page's line")
	}
}
