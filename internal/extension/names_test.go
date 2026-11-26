package extension

import (
	"context"
	"strings"
	"testing"

	"github.com/nwverse/persona/sdk/go/wire"

	"github.com/nwverse/personal-agent-club/internal/core"
	"github.com/nwverse/personal-agent-club/internal/protocol"
	"github.com/nwverse/personal-agent-club/internal/provider/echo"
)

// Pages name an account in their own language (accountTitle); the assistant's tools and a takeover's machine in
// Chinese (accountName). Neither ever shows the raw provider id.
func TestAccountAndProviderNamesAreHuman(t *testing.T) {
	duet := protocol.Account{Provider: "claude_duet", DisplayName: "Claude Duet · Ada", DisplayNameEn: "Claude Duet · Ada"}
	muse := protocol.Account{Provider: "muse", DisplayName: "Muse（网页版） · Glint", DisplayNameEn: "Muse (web) · Glint"}
	bare := protocol.Account{Provider: "gemini_tasks"}
	raw := protocol.Account{Provider: "muse2api", DisplayName: "muse2api"}
	for _, c := range []struct {
		l    lang
		a    protocol.Account
		page string
	}{
		{"zh-Hans", muse, "Muse（网页版） · Glint"}, {"en", muse, "Muse (web) · Glint"}, {"en", duet, "Claude Duet · Ada"},
		{"zh", bare, "Gemini"}, {"en", bare, "Gemini"}, {"en", raw, "Muse (own relay)"}, {"zh", raw, "Muse（自建转接）"},
	} {
		if got := accountTitle(c.l, c.a); got != c.page {
			t.Errorf("accountTitle(%s, %+v) = %q, want %q", c.l, c.a, got, c.page)
		}
	}
	if accountName(muse) != "Muse（网页版） · Glint" || accountName(raw) != "Muse（自建转接）" || accountName(bare) != "Gemini" {
		t.Fatal(accountName(muse), accountName(raw), accountName(bare))
	}
	for _, c := range []struct {
		l              lang
		provider, want string
	}{{"zh-Hans", "muse", "Muse（网页版）"}, {"en", "muse", "Muse (web)"}, {"zh", "echo", "演示账号"}, {"en", "echo", "Demo account"},
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
