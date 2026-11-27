package extension

import (
	"strings"
	"testing"
)

// Every text here is made up for the test.
func TestAsksForAuthRecognisesRequests(t *testing.T) {
	for name, text := range map[string]string{
		"tailscale link, Chinese":    "这台机器要先加入网络。请打开 https://login.tailscale.com/a/0a1b2c3d4e5f 批准一下。",
		"google oauth, English":      "To continue, visit https://accounts.google.com/o/oauth2/v2/auth?client_id=demo&scope=x and approve access.",
		"github device and a code":   "Open https://github.com/login/device and enter the code ABCD-1234.",
		"device code at any link":    "Go to https://example.org/activate and type the code WXYZ-9876 to link this machine.",
		"oauth path on another host": "Please click https://auth.example.net/oauth/authorize?id=1 to approve.",
		"words only: asks to log in": "任务停在登录这一步，需要你先登录。",
		"words only: link in words":  "你点一下这个链接批准就行，我等着。",
		"words only: expired":        "登录已过期，我没法继续了。",
		"words only: waiting":        "我在等你授权。",
		"link right after Chinese":   "请到https://login.tailscale.com/a/abc123批准一下",
		"expired, with a link":       "你的登录过期了，请打开 https://login.tailscale.com/a/abc123 重新登录。",
		"scan to log in":             "麻烦你扫一下码登录。",
		"could you":                  "Could you sign in at https://portal.example.com/sso/start?",
		"need you to":                "I need you to log in before I can go on.",
		"sign back in":               "Please sign back in.",
		"re-auth with a code":        "Please re-authenticate: open https://example.org/device and enter the code WDJB-MJHT.",
		"english please log in":      "Please log in again to continue.",
		"english you need to":        "You need to sign in on the page that opened.",
		"english click this link":    "Click this link to approve the request, then tell me.",
		"english login required":     "Login required: the session timed out.",
		"english waiting":            "I'm waiting for you to sign in.",
		"markdown link":              "[批准登录](https://login.tailscale.com/a/abc123) 然后告诉我",
	} {
		t.Run(name, func(t *testing.T) {
			if !asksForAuth(text) {
				t.Fatalf("not recognised: %q", text)
			}
		})
	}
}

func TestAsksForAuthStaysQuietOtherwise(t *testing.T) {
	long := strings.Repeat("这是一段很长的说明文字，讲的是系统是怎么工作的。", 40) + "如果失败，请重新登录。"
	for name, text := range map[string]string{
		"empty":                     "",
		"already signed in":         "我已经登录好了，可以继续。",
		"no need":                   "不需要登录，直接用就行。",
		"if needed":                 "如果需要登录，我会告诉你。",
		"english no need":           "There is no need to log in for this.",
		"english if":                "If you sign in later, the history will sync.",
		"how to, no ask":            "如何登录 Tailscale：运行 tailscale up 即可。",
		"doc link, no ask":          "参考 https://accounts.google.com/signin/help 了解更多。",
		"ordinary link":             "构建通过了。可以在 https://github.com/org/repo/pull/3 看改动。",
		"login page, not a sign-in": "The login page is at https://example.com/login and it is slow.",
		"api talk":                  "Authentication is required by that API, and I used the token already set.",
		"code block example":        "示例：\n```\nplease log in\nhttps://login.tailscale.com/a/abc approve\n```\n这是脚本里的提示。",
		"quoted passage":            "他们的提示是：\n> Please log in again to continue.\n我没遇到。",
		"long explanation":          long,
		"code pair without a link":  "build ABCD-1234 failed, enter debug mode",
		"a link and no verb":        "https://login.tailscale.com/a/abc123",
		"verb and a non-login link": "Open https://example.com/docs/intro for the intro.",
		"thanks":                    "谢谢，已经处理好了。",
	} {
		t.Run(name, func(t *testing.T) {
			if asksForAuth(text) {
				t.Fatalf("taken for a request: %q", text)
			}
		})
	}
}
