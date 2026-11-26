package extension

import (
	"testing"

	"github.com/nwverse/personal-agent-club/internal/protocol"
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
