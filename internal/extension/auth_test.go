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
		"ask, then the link":         "Can you approve the device? https://login.tailscale.com/a/abc123",
		"a deep authorize endpoint":  "Authorize me here: https://slack.com/oauth/v2/authorize?client_id=1&scope=x",
		"你得先登录":                      "这一步我做不了，你得先登录一下。",
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
		"empty":                      "",
		"already signed in":          "我已经登录好了，可以继续。",
		"no need":                    "不需要登录，直接用就行。",
		"if needed":                  "如果需要登录，我会告诉你。",
		"english no need":            "There is no need to log in for this.",
		"english if":                 "If you sign in later, the history will sync.",
		"how to, no ask":             "如何登录 Tailscale：运行 tailscale up 即可。",
		"doc link, no ask":           "参考 https://accounts.google.com/signin/help 了解更多。",
		"ordinary link":              "构建通过了。可以在 https://github.com/org/repo/pull/3 看改动。",
		"login page, not a sign-in":  "The login page is at https://example.com/login and it is slow.",
		"api talk":                   "Authentication is required by that API, and I used the token already set.",
		"code block example":         "示例：\n```\nplease log in\nhttps://login.tailscale.com/a/abc approve\n```\n这是脚本里的提示。",
		"quoted passage":             "他们的提示是：\n> Please log in again to continue.\n我没遇到。",
		"long explanation":           long,
		"code pair without a link":   "build ABCD-1234 failed, enter debug mode",
		"a link and no verb":         "https://login.tailscale.com/a/abc123",
		"verb and a non-login link":  "Open https://example.com/docs/intro for the intro.",
		"thanks":                     "谢谢，已经处理好了。",
		"the AI must log in itself":  "我需要先登录 GitHub 才能看这个仓库，稍等。",
		"the AI will log in":         "我得先登录一下控制台。",
		"already done, with a link":  "我已经用 https://accounts.google.com/o/oauth2/auth 登录好了，你可以继续。",
		"an old failure":             "上次失败是因为登录已过期，我刷新了凭据，现在正常。",
		"a quoted error":             "报错信息是 \"login required\"，我换了个接口就好了。",
		"approve a pull request":     "Please approve the PR when you have a minute.",
		"waiting for approval":       "Waiting for your approval before I start the deploy.",
		"我在等你批准":                     "方案写好了，我在等你批准。",
		"oauth guide":                "You can open https://docs.example.com/guide/oauth/ to read how it works.",
		"source tree":                "Please open https://github.com/org/repo/tree/main/device/ and review the driver.",
		"ticket id":                  "See PROJ-1234 at https://tracker.example.com/browse/PROJ-1234 and type hints are missing.",
		"steps for the admin page":   "登录步骤：\n1. 打开 https://login.tailscale.com/admin/machines\n2. 找到这台机器",
		"signed in successfully":     "You are all set: I signed in successfully at https://accounts.google.com/o/oauth2/auth for you.",
		"after you log in":           "After you log in next week, the history will be there.",
		"oauth doc, open if curious": "See https://docs.example.com/guide/oauth/ for how OAuth works; open it if curious.",
		"a folder named oauth":       "我把你的授权文件放在 https://example.com/oauth 下面了，你打开看看内容对不对。",
		"a tip with an if":           "Tip: if your session expires, please log in again from the menu.",
		"advice for later":           "Note for later: you will need to sign in to the new dashboard once it ships next month.",
		"steps for other people":     "Next step for the users: open https://app.example.com/activate and enter the code shown on their TV.",
		"a how-to":                   "你之前问怎么登录 Tailscale：装好之后运行 tailscale up，会弹出 https://login.tailscale.com/a/abc 让你批准。",
		"你 alone is not an ask":      "这是你上次用过的链接 https://login.tailscale.com/a/abc123 ，打开后是空白页，我在查原因。",
	} {
		t.Run(name, func(t *testing.T) {
			if asksForAuth(text) {
				t.Fatalf("taken for a request: %q", text)
			}
		})
	}
}
