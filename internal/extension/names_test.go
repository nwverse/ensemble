package extension

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/nwverse/persona/sdk/go/exttest"
	"github.com/nwverse/persona/sdk/go/wire"

	pac "github.com/nwverse/ensemble"
	"github.com/nwverse/ensemble/internal/core"
	"github.com/nwverse/ensemble/internal/protocol"
	"github.com/nwverse/ensemble/internal/provider"
	"github.com/nwverse/ensemble/internal/provider/echo"
)

// Pages name an account in their own language (accountTitle); the assistant's tools and a takeover's machine in
// Chinese (accountName). Neither ever shows the raw provider id.
func TestAccountAndProviderNamesAreHuman(t *testing.T) {
	duet := protocol.Account{Provider: "claude_duet", DisplayName: "Claude Duet · Ada", DisplayNameEn: "Claude Duet · Ada"}
	muse := protocol.Account{Provider: "muse", DisplayName: "Muse 网页版 · Wren", DisplayNameEn: "Muse (web) · Wren"}
	bare := protocol.Account{Provider: "gemini_tasks"}
	raw := protocol.Account{Provider: "muse2api", DisplayName: "muse2api"}
	for _, c := range []struct {
		l    lang
		a    protocol.Account
		page string
	}{
		{"zh-Hans", muse, "Muse 网页版 · Wren"}, {"en", muse, "Muse (web) · Wren"}, {"en", duet, "Claude Duet · Ada"},
		{"zh", bare, "Gemini"}, {"en", bare, "Gemini"}, {"en", raw, "Muse (own relay)"}, {"zh", raw, "Muse 自建转接"},
	} {
		if got := accountTitle(c.l, c.a); got != c.page {
			t.Errorf("accountTitle(%s, %+v) = %q, want %q", c.l, c.a, got, c.page)
		}
	}
	if accountName(muse) != "Muse 网页版 · Wren" || accountName(raw) != "Muse 自建转接" || accountName(bare) != "Gemini" {
		t.Fatal(accountName(muse), accountName(raw), accountName(bare))
	}
	for _, c := range []struct {
		l              lang
		provider, want string
	}{{"zh-Hans", "muse", "Muse 网页版"}, {"en", "muse", "Muse (web)"}, {"zh", "echo", "演示账号"}, {"en", "echo", "Demo account"},
		{"en", "muse2api", "Muse (own relay)"}, {"en", "dots", "ChatGPT Dots"}, {"zh", "gemini_tasks", "Gemini"}} {
		if got := providerName(c.l, c.provider); got != c.want {
			t.Errorf("providerName(%s, %s) = %q, want %q", c.l, c.provider, got, c.want)
		}
	}
}

// renameField is the account page's name field.
func renameField(t *testing.T, v *wire.View) wire.Component {
	t.Helper()
	for _, s := range v.Sections {
		for _, c := range s.Items {
			if c.Type == "field" && c.Name == fieldName {
				return c
			}
		}
	}
	t.Fatalf("no name field: %+v", v)
	return wire.Component{}
}

// The rename field holds only a name the owner chose; a composed title ("演示账号 (2)", a brand in another language)
// is its placeholder, so pressing 改名 unchanged cannot freeze it as theirs.
func TestRenameFieldIsPrefilledOnlyWithTheOwnersName(t *testing.T) {
	h, club := start(t)
	ctx := context.Background()
	space, _ := club.Space(ctx, core.DefaultSpace)
	for _, cred := range []string{demoCredential, demoCredential + "-2"} {
		if _, err := space.AddAccount(ctx, protocol.AddAccountParams{Provider: echo.Name, Credential: cred}); err != nil {
			t.Fatal(err)
		}
	}
	st, _ := space.Status(ctx)
	id := st.Accounts[1].AccountID
	f := renameField(t, render(t, h, pageState{Page: pageAccount, Account: id}))
	if f.Value != "" || f.Placeholder != "演示账号 (2)" {
		t.Fatalf("composed name prefilled: %+v", f)
	}
	// 「改名」 pressed with the field as shown
	if res := act(t, h, pageState{Page: pageAccount, Account: id}, actRename, map[string]string{fieldName: f.Value}); strings.Contains(res.Toast, "已改名") {
		t.Fatalf("an unchanged field renamed: %+v", res)
	}
	if a, _ := space.Account(ctx, id); a.OwnerName != "" {
		t.Fatalf("frozen as the owner's: %+v", a)
	}
	name := "Grok Bot (2)"
	if _, err := space.UpdateAccount(ctx, protocol.UpdateAccountParams{AccountID: id, DisplayName: &name}); err != nil {
		t.Fatal(err)
	}
	if f := renameField(t, render(t, h, pageState{Page: pageAccount, Account: id})); f.Value != name {
		t.Fatalf("owner name not prefilled: %+v", f)
	}
}

// emailGrok is a Grok Bot whose Verify gives the e-mail after the credential's "|" as the account's alias, as GetMe
// does for an account without a single bot.
type emailGrok struct{ *fakeGrok }

func (emailGrok) Verify(_ context.Context, cred string) (provider.RemoteAccount, error) {
	_, email, _ := strings.Cut(cred, "|")
	return provider.RemoteAccount{ExternalAccountID: "grok-" + email, Alias: email}, nil
}

// renderIn is render in a page language.
func renderIn(t *testing.T, h *exttest.Host, locale string, st pageState) *wire.View {
	t.Helper()
	var v wire.View
	if err := h.Call("ui.render", wire.UIParams{Contribution: AccountsUI, Placement: wire.PlacementPluginPage, Size: "full",
		UIVersion: 6, Locale: locale, State: stateOf(st)}, &v); err != nil {
		t.Fatalf("ui.render %+v: %v", st, err)
	}
	checkView(t, &v)
	return &v
}

// infoRow is the value of the account page's row titled title, "" when there is none.
func infoRow(v *wire.View, title string) string {
	for _, s := range v.Sections {
		for _, c := range s.Items {
			if c.Type == "row" && c.Title == title {
				return c.Value
			}
		}
	}
	return ""
}

// An account named after its e-mail is titled by the part before @ on the list and its own page, in Chinese and
// English; its page keeps the whole e-mail as a row. An e-mail the owner typed as the name is shown as typed, with no
// extra row.
func TestEmailNamedAccountsShowTheShortNameAndKeepTheEmailOnTheirPage(t *testing.T) {
	club := core.New(core.Options{Registry: provider.NewRegistry(emailGrok{&fakeGrok{}}), FollowUps: []time.Duration{}, WatchLimit: -1})
	h := startHost(t, pac.Manifest, exttest.Serve(t, Program(club, pac.Version())), json.RawMessage(`{}`))
	ctx := context.Background()
	space, _ := club.Space(ctx, core.DefaultSpace)
	auto, err := space.AddAccount(ctx, protocol.AddAccountParams{Provider: grokBot, Credential: grokCredential + "|someone@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	owned, err := space.AddAccount(ctx, protocol.AddAccountParams{Provider: grokBot, Credential: grokCredential + "|other@example.com",
		DisplayName: "other@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ locale, emailRow string }{{"zh-Hans", "邮箱"}, {"en", "Email"}} {
		list := text(renderIn(t, h, c.locale, pageState{}))
		if !strings.Contains(list, "Grok Bot · someone") || strings.Contains(list, "someone@example.com") {
			t.Errorf("%s list: %s", c.locale, list)
		}
		page := renderIn(t, h, c.locale, pageState{Page: pageAccount, Account: auto.AccountID})
		if page.Title != "Grok Bot · someone" || infoRow(page, c.emailRow) != "someone@example.com" {
			t.Errorf("%s account page: %q, %s row %q", c.locale, page.Title, c.emailRow, infoRow(page, c.emailRow))
		}
		if f := renameField(t, page); f.Value != "" || f.Placeholder != "Grok Bot · someone" {
			t.Errorf("%s rename field: %+v", c.locale, f)
		}
		page = renderIn(t, h, c.locale, pageState{Page: pageAccount, Account: owned.AccountID})
		if page.Title != "other@example.com" || infoRow(page, c.emailRow) != "" || renameField(t, page).Value != "other@example.com" {
			t.Errorf("%s owner's page: %q, %s row %q", c.locale, page.Title, c.emailRow, infoRow(page, c.emailRow))
		}
	}
}

// The pages name the web and the self-hosted Muse with the labels' table, in the rows to add one and on the
// self-hosted one's page that sends people to the web one. The relay's row says first that it is not Muse's own and
// sends an ordinary Muse account to the web one.
func TestMuseLabelsOnThePages(t *testing.T) {
	h, _ := start(t)
	for _, c := range []struct{ locale, web, relay, notMuse string }{
		{"zh-Hans", "Muse 网页版", "Muse 自建转接", "不是 Muse 官方的："}, {"en", "Muse (web)", "Muse (own relay)", "Not Muse's own: "}} {
		add := renderIn(t, h, c.locale, pageState{Page: pageAdd})
		list := text(add)
		if !strings.Contains(list, `"title":"`+c.web+`"`) || !strings.Contains(list, `"title":"`+c.relay+`"`) || strings.Contains(list, "（网页版）") {
			t.Errorf("%s list: %s", c.locale, list)
		}
		found := false
		for _, s := range add.Sections {
			for _, it := range s.Items {
				if it.Key == "pair-muse2api" {
					found = true
					if !strings.HasPrefix(it.Subtitle, c.notMuse) || !strings.Contains(it.Subtitle, c.web) {
						t.Errorf("%s relay row: %q", c.locale, it.Subtitle)
					}
				}
			}
		}
		if !found {
			t.Errorf("%s: no relay row", c.locale)
		}
		relay := text(renderIn(t, h, c.locale, pageState{Page: pagePair, Provider: "muse2api"}))
		if !strings.Contains(relay, c.web) || strings.Contains(relay, "（网页版）") {
			t.Errorf("%s muse2api page: %s", c.locale, relay)
		}
	}
}
