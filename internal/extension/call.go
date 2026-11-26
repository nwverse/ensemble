package extension

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/nwverse/personal-agent-club/internal/core"
	"github.com/nwverse/personal-agent-club/internal/protocol"
	"github.com/nwverse/personal-agent-club/internal/provider"
)

// call answers method in the default space through core.Club.Call, the path every shell uses, and decodes its answer
// into out (nil: dropped).
func call(ctx context.Context, club *core.Club, method string, params, out any) error {
	raw, err := json.Marshal(params)
	if err != nil {
		return err
	}
	res, err := club.Call(ctx, core.DefaultSpace, method, raw)
	if err != nil {
		return err
	}
	if out == nil {
		return nil
	}
	b, err := json.Marshal(res)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, out)
}

// reason is an error in words for the page: what went wrong and what to do next, in everyday words. The core's own
// message (English, for developers) is only added, as a detail, when the error has no words of its own here. secret,
// when given, is never repeated (a credential the user just typed).
func reason(l lang, err error, secret string) string {
	msg := err.Error()
	if secret != "" && strings.Contains(msg, secret) {
		msg = strings.ReplaceAll(msg, secret, "***")
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return l.t("等太久没有回应，请稍后再试。", "Timed out; try again later.")
	}
	if words := reasonWords(l, protocol.ReasonOf(err)); words != "" {
		return words
	}
	detail := l.t("（详情：", " (detail: ") + msg + l.t("）", ")")
	switch protocol.CodeOf(err) {
	case protocol.CodeUnsupported:
		return l.t("暂时做不到这件事。", "This cannot be done for now.") + detail
	case protocol.CodeInvalid:
		return l.t("填写的内容不对，请检查后再试。", "Something entered is not right; check it and try again.") + detail
	case protocol.CodeNotFound:
		return l.t("找不到要操作的东西，它可能已经被移除了，请返回刷新一下。", "Not found; it may have been removed. Go back and refresh.") + detail
	case protocol.CodeConflict:
		return l.t("现在不能这样做。", "This cannot be done right now.") + detail
	}
	return l.t("没有成功，请稍后再试。", "That did not work; try again later.") + detail
}

// reasonWords are the words for a refusal's reason (protocol reasons and the provider codes a refused pairing
// carries), "" for one without words here.
func reasonWords(l lang, why string) string {
	switch why {
	case protocol.ReasonAlreadyPaired:
		return l.t("这个账号已经添加过了，不用再加：在「已接入的账号」里能找到它。",
			"This account is already added; find it under Your accounts.")
	case protocol.ReasonProviderUnavailable:
		return l.t("这个服务现在还没打开：请按页面上的说明到扩展的设置里打开它，再回来添加。",
			"This service is not switched on; switch it on in the extension's settings as the page says, then come back.")
	case protocol.ReasonAccountPaused:
		return l.t("这个账号已暂停同步：先在它的页面点「恢复同步」。", "This account is paused: resume it on its page first.")
	case protocol.ReasonAccountNotFound:
		return l.t("找不到这个账号了，它可能已经被移除。", "This account is gone; it may have been removed.")
	case protocol.ReasonAccountMismatch:
		return l.t("这次登录的是另一个账号，不是这一个：请用原来的账号重新登录。",
			"That sign-in was a different account; sign in with the original one.")
	case protocol.ReasonLoginLimit:
		return l.t("已有两个登录在等待：先取消一个，或等它们 10 分钟后过期。",
			"Two sign-ins are already waiting: cancel one, or wait for them to expire after 10 minutes.")
	case protocol.ReasonLoginNotFound:
		return l.t("这次登录已过期（10 分钟）或已取消，请重新开始。", "This sign-in expired (after 10 minutes) or was canceled; start again.")
	case provider.CodeAuthFailed:
		return l.t("对方不认这个登录：密钥可能没复制完整或已经作废，请回对方网站重新复制一串再试。",
			"The service did not accept it: the key may be cut off or revoked; copy it again from the service and retry.")
	case provider.CodeRateLimited:
		return l.t("对方暂时限制了访问次数，请过几分钟再试。", "The service is limiting requests for now; try again in a few minutes.")
	case provider.CodeNetworkError, "SYNC_FAILED":
		return l.t("连不上对方：请确认网络正常（用电脑浏览器的，确认那台电脑开着并在线），稍后再试。",
			"Could not reach the service: check the network (for a browser account, that the computer is on and online) and try again.")
	case provider.CodeUpstreamError, provider.CodeUnavailable, provider.CodeBadResponse, provider.CodeRequestFailed:
		return l.t("对方网站暂时出错了，请稍后再试。", "The service had a problem; try again later.")
	}
	return ""
}

// lang picks the words for a locale: Chinese for zh-*, and the hub's default (zh-Hans) when there is none; English
// otherwise.
type lang string

func (l lang) t(zh, en string) string {
	if l == "" || strings.HasPrefix(string(l), "zh") {
		return zh
	}
	return en
}
