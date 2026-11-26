package runtime

import (
	"testing"

	"github.com/nwverse/personal-agent-club/internal/protocol"
)

// A launcher and an assistant session are named after the account; an account the space gives no name is named
// after its provider's brand, never its id.
func TestLauncherAndSessionTitlesNeverFallBackToAProviderID(t *testing.T) {
	for _, c := range []struct{ provider, name, want string }{
		{"claude_duet", "", "Claude Duet"},
		{"gemini_tasks", "", "Gemini"},
		{"gemini_tasks", "gemini_tasks", "Gemini"},
		{"grok_bot", "", "Grok Bot"},
		{"muse2api", "", "Muse 自建转接"}, {"my_site", "", "My Site"},
		{"echo", "", "演示账号"},
		{"claude_duet", "Claude Duet · Ada", "Claude Duet · Ada"},
		{"grok_bot", "Grok Bot (2)", "Grok Bot (2)"},
		{"cue", "Cue Relay", "Cue Relay"},
	} {
		acct := protocol.Account{AccountID: "a0123456789ab", Provider: c.provider, DisplayName: c.name}
		if got := accountTitle(acct); got != c.want {
			t.Errorf("accountTitle(%s, %q) = %q, want %q", c.provider, c.name, got, c.want)
		}
		b := &Bridge{accounts: []accountTarget{{account: acct, launcher: LauncherName(c.provider, acct.AccountID)}}}
		l := b.launcherSnapshot()
		if len(l) != 1 || l[0].DisplayName != c.want || l[0].Agent != c.provider {
			t.Errorf("launcher %+v, want display name %q and agent %q", l, c.want, c.provider)
		}
	}
}
