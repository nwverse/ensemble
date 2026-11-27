package extension

import (
	"cmp"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/nwverse/persona/sdk/go/wire"

	"github.com/nwverse/ensemble/internal/protocol"
	"github.com/nwverse/ensemble/internal/provider/cue"
	"github.com/nwverse/ensemble/internal/provider/echo"
	"github.com/nwverse/ensemble/internal/provider/groq"
	"github.com/nwverse/ensemble/internal/provider/muse2api"
)

// quotaExhaustedActive reports whether any active quota dimension is currently exhausted (matching core's
// pre-send guard: multi-bucket aware and respecting staleness for no-reset cooldowns). When now > 0, expired
// resets are ignored too.
func quotaExhaustedActive(q *protocol.Quota, now int64) (bool, int64) {
	if q == nil {
		return false, 0
	}
	if len(q.Buckets) > 0 {
		var best int64
		found := false
		for _, b := range q.Buckets {
			if b.State != protocol.QuotaExhausted {
				continue
			}
			if b.ResetsAt > 0 {
				if now > 0 && now >= b.ResetsAt {
					continue
				}
				if !found || b.ResetsAt > best {
					best = b.ResetsAt
				}
				found = true
			} else if !q.Stale {
				found = true
			}
		}
		return found, best
	}
	if q.State != protocol.QuotaExhausted {
		return false, 0
	}
	if q.ResetsAt > 0 {
		if now > 0 && now >= q.ResetsAt {
			return false, 0
		}
		return true, q.ResetsAt
	}
	return !q.Stale, 0
}

// quotaDisplaySection renders the account detail page's allowance & usage block:
//   - when real quota readings or a rate-limit cooldown exist: status, remaining %, reset time, per-window
//     progress bars, data source + check time, pre-send warning when exhausted, and avatar ring selector;
//   - when the service has not reported real numbers: honest local send-count estimation (5h/24h) and a plain
//     explanation of the source and estimation method for each service.
func quotaDisplaySection(l lang, acct protocol.Account, c clock) wire.Section {
	if acct.Quota == nil {
		return quotaEstimatedSection(l, acct)
	}
	val := acct.QuotaDisplay
	if val == "" {
		if !acct.ShowQuota {
			val = protocol.QuotaDisplayOff
		} else {
			val = protocol.QuotaDisplayAuto
		}
	}
	choices := []wire.Choice{
		{Value: protocol.QuotaDisplayAuto, Title: quotaChoiceTitle(l, protocol.QuotaDisplayAuto, "", nil)},
	}
	var items []wire.Component
	if activeExhausted, _ := quotaExhaustedActive(acct.Quota, c.now.UnixMilli()); activeExhausted {
		items = append(items, wire.Component{
			Type:     "row",
			Key:      "quota-warn",
			Icon:     "warning",
			Title:    l.t("发消息前会先提示拦截", "Sends pause with a warning until reset"),
			Subtitle: l.t("这个账号的额度已经用完，继续发消息会先给出明确提示，避免静默失败。如果确认已经恢复，点上面的「立即同步」刷新。", "This account's allowance is used up; sends show a clear notice before going out instead of failing silently. Tap Sync now above if it already reset."),
			Badge:    &wire.Badge{Text: l.t("额度用完", "Used up"), Tone: wire.ToneDanger},
		})
	} else if acct.Quota.State == protocol.QuotaLow {
		items = append(items, wire.Component{
			Type:     "row",
			Key:      "quota-warn",
			Icon:     "warning",
			Title:    l.t("额度快用完了", "Allowance is running low"),
			Subtitle: l.t("剩余不足 20%，发大段任务前留意下面的重置时间。", "Less than 20% left; check the reset time below before starting heavy tasks."),
			Badge:    &wire.Badge{Text: l.t("快用完", "Low"), Tone: wire.ToneCaution},
		})
	}

	value := quotaWords(l, acct.Quota.Kind, "", acct.Quota.Percent)
	for _, b := range acct.Quota.Buckets {
		if b.ID == acct.Quota.Selected {
			value = quotaWords(l, b.Kind, b.Window, b.Percent)
		}
	}
	if acct.Quota.State == protocol.QuotaExhausted {
		value = l.t("用完了", "Used up")
		if acct.Quota.ResetsAt > 0 {
			back := c.at(l, acct.Quota.ResetsAt)
			if c.relative(acct.Quota.ResetsAt) {
				value += l.t("，"+back+"恢复", ", back "+back)
			} else {
				value += l.t("，"+back+" 恢复", ", back on "+back)
			}
		}
	}
	badgeText := l.t("剩余", "left")
	if acct.Quota.State == protocol.QuotaExhausted {
		badgeText = l.t("已用完", "Used up")
	} else if acct.Quota.Percent != nil {
		if acct.Quota.State == protocol.QuotaLow {
			badgeText = fmt.Sprintf(l.t("仅剩 %d%%", "%d%% left"), *acct.Quota.Percent)
		} else {
			badgeText = fmt.Sprintf("%d%%", *acct.Quota.Percent)
		}
	}
	items = append(items, wire.Component{Type: "row", Title: l.t("现在还剩", "Left now"), Value: value,
		Badge: &wire.Badge{Text: badgeText, Tone: quotaBadgeTone(acct.Quota.Tone)}})

	if acct.Quota.ResetsAt > 0 {
		items = append(items, wire.Component{
			Type:  "row",
			Title: l.t("重置时间", "Resets"),
			Value: quotaResetWords(l, c, acct.Quota.ResetsAt),
		})
	}

	bars := map[string]bool{}
	for i, b := range acct.Quota.Buckets {
		key := "quota-bucket-" + b.ID
		if b.ID == "" || bars[key] { // a key is once a page (wire.CheckView)
			key = fmt.Sprintf("quota-bucket-%d-%s", i, b.ID)
		}
		bars[key] = true
		ratio, _ := quotaBucketRatio(b)
		items = append(items, wire.Component{
			Type:   "progress",
			Key:    key,
			Label:  quotaWords(l, b.Kind, b.Window, b.Percent),
			Ratio:  &ratio,
			Tone:   quotaBadgeTone(b.Tone),
			Detail: quotaBucketDetail(l, b, c),
		})
	}

	srcSub := ""
	if acct.Quota.CheckedAt > 0 {
		srcSub = l.t("上次检查：", "Checked ") + c.at(l, acct.Quota.CheckedAt)
		if acct.Quota.Stale {
			srcSub += l.t("（已超过 15 分钟未刷新，可点「立即同步」更新）", " (over 15 minutes old; tap Sync now to refresh)")
		}
	}
	items = append(items, wire.Component{
		Type:     "row",
		Title:    l.t("数据来源", "Source"),
		Value:    quotaSourceLabel(l, acct.Provider, acct.Quota),
		Subtitle: srcSub,
	})

	choices = append(choices, quotaBucketChoices(l, acct.Quota.Buckets, val)...)
	if val != protocol.QuotaDisplayAuto && val != protocol.QuotaDisplayOff && !slices.ContainsFunc(choices, func(c wire.Choice) bool { return c.Value == val }) {
		choices = append(choices, wire.Choice{Value: val, Title: quotaChoiceTitle(l, quotaChoiceCurrent, "", nil)})
	}
	choices = append(choices, wire.Choice{Value: protocol.QuotaDisplayOff, Title: quotaChoiceTitle(l, protocol.QuotaDisplayOff, "", nil)})
	items = append(items, wire.Component{
		Type:    "segmented",
		Name:    fieldQuotaDisplay,
		Choices: choices,
		Value:   val,
		Action:  &wire.Action{ID: actQuotaDisplay},
	})
	return wire.Section{
		Title: l.t("额度与用量", "Allowance & usage"),
		Footer: l.t("有的服务限制你每几小时、每天或每周能用多少。Persona 会在这个账号的头像外画一圈「电量」，表示还剩多少；上面可以选画哪一项。",
			"Some services limit how much you can use per window, day or week. Persona draws a battery ring around the avatar for what is left; choose which one above."),
		Items: items,
	}
}

// The hub (wire.CheckView) and the schema hold a segmented control to 2–5 choices of at most 12 characters each: a
// wider one is refused and the whole page fails with HTTP 502. Auto and Off always take two of the five.
const (
	segmentedTitleMost    = 12
	segmentedChoicesMost  = 5
	quotaBucketChoiceMost = segmentedChoicesMost - 2
	// quotaChoiceCurrent is quotaChoiceTitle's kind for a display choice saved earlier that no bucket reports now.
	quotaChoiceCurrent = "\x00current"
)

// quotaBucketRatio is how much of a bucket is left, 0–1, and whether the bucket says at all.
func quotaBucketRatio(b protocol.QuotaBucket) (float64, bool) {
	switch {
	case b.Remaining != nil:
		return math.Max(0, math.Min(1, *b.Remaining)), true
	case b.Percent != nil:
		return math.Max(0, math.Min(1, float64(*b.Percent)/100)), true
	}
	return 0, false
}

// quotaBucketChoices are the ring selector's choices for the buckets, in their order: one per bucket id, at most
// quotaBucketChoiceMost (one fewer when val is a choice no bucket has, which takes a place of its own). When there are
// more, the chosen one stays and then the ones with the least left. Titles are short (quotaChoiceTitle) and told apart.
func quotaBucketChoices(l lang, buckets []protocol.QuotaBucket, val string) []wire.Choice {
	var uniq []protocol.QuotaBucket
	seen := map[string]bool{protocol.QuotaDisplayAuto: true, protocol.QuotaDisplayOff: true}
	chosen := false
	for _, b := range buckets {
		if b.ID == "" || seen[b.ID] {
			continue
		}
		seen[b.ID] = true
		uniq = append(uniq, b)
		chosen = chosen || b.ID == val
	}
	most := quotaBucketChoiceMost
	if !chosen && val != protocol.QuotaDisplayAuto && val != protocol.QuotaDisplayOff {
		most-- // the choice saved before takes a place
	}
	if len(uniq) > most {
		rank := make([]int, len(uniq))
		for i := range rank {
			rank[i] = i
		}
		slices.SortStableFunc(rank, func(i, j int) int {
			if (uniq[i].ID == val) != (uniq[j].ID == val) {
				if uniq[i].ID == val {
					return -1
				}
				return 1
			}
			ri, oki := quotaBucketRatio(uniq[i])
			rj, okj := quotaBucketRatio(uniq[j])
			if oki != okj { // a bucket that says how much is left comes before one that does not
				if oki {
					return -1
				}
				return 1
			}
			return cmp.Compare(ri, rj)
		})
		keep := map[int]bool{}
		for _, i := range rank[:most] {
			keep[i] = true
		}
		var kept []protocol.QuotaBucket
		for i, b := range uniq {
			if keep[i] {
				kept = append(kept, b)
			}
		}
		uniq = kept
	}
	titles := map[string]bool{
		quotaChoiceTitle(l, protocol.QuotaDisplayAuto, "", nil): true,
		quotaChoiceTitle(l, protocol.QuotaDisplayOff, "", nil):  true,
		quotaChoiceTitle(l, quotaChoiceCurrent, "", nil):        true,
	}
	out := make([]wire.Choice, 0, len(uniq))
	for _, b := range uniq {
		title := quotaChoiceTitle(l, b.Kind, b.Window, b.Percent)
		for n := 2; titles[title]; n++ { // two buckets that read alike: number the later one
			title = quotaFit(quotaChoiceTitle(l, b.Kind, b.Window, nil), fmt.Sprintf(" %d", n))
		}
		titles[title] = true
		out = append(out, wire.Choice{Value: b.ID, Title: title})
	}
	return out
}

// quotaChoiceTitle is a ring selector choice's title, at most 12 characters (wire.CheckView): Auto, Off, the choice
// saved before (kind quotaChoiceCurrent), or a bucket as its window, kind and percent left (「5小时 91%」「5h 91%」).
// The long wording (quotaWords) stays on the rows and bars above it.
func quotaChoiceTitle(l lang, kind, window string, pct *int) string {
	switch kind {
	case protocol.QuotaDisplayAuto:
		return l.t("自动", "Auto")
	case protocol.QuotaDisplayOff:
		return l.t("不显示", "Off")
	case quotaChoiceCurrent:
		return l.t("当前项", "Current")
	}
	var zhWin, enWin, enUnit string
	switch window {
	case "":
	case "minute":
		zhWin, enWin, enUnit = "每分", "1m", "min"
	case "hour":
		zhWin, enWin, enUnit = "每时", "1h", "h"
	case "3h":
		zhWin, enWin, enUnit = "3小时", "3h", "3h"
	case "5h":
		zhWin, enWin, enUnit = "5小时", "5h", "5h"
	case "day":
		zhWin, enWin, enUnit = "每天", "Day", "day"
	case "week":
		zhWin, enWin, enUnit = "每周", "Week", "wk"
	case "month":
		zhWin, enWin, enUnit = "每月", "Month", "mo"
	default:
		zhWin, enWin, enUnit = window, window, window
		if n, ok := strings.CutSuffix(window, "h"); ok && n != "" && strings.Trim(n, "0123456789") == "" {
			zhWin = n + "小时"
		} else if n, ok := strings.CutSuffix(window, "d"); ok && n != "" && strings.Trim(n, "0123456789") == "" {
			zhWin = n + "天"
		}
	}
	var zhKind, enKind string
	switch kind {
	case protocol.QuotaKindCredits:
		zhKind, enKind = "点数", "Credit"
	case protocol.QuotaKindRequests:
		zhKind, enKind = "次数", "Req"
	case protocol.QuotaKindTokens:
		zhKind, enKind = "字数", "Tok"
	}
	zh, en := zhWin+zhKind, enWin
	switch {
	case enKind == "":
	case kind == protocol.QuotaKindCredits || enUnit == "":
		en = enKind // credits are told from the windows by their kind; 「Credit/mo」 would not fit with a percent
	default:
		en = enKind + "/" + enUnit
	}
	if zh == "" {
		zh = "额度"
	}
	if en == "" {
		en = "Quota"
	}
	suffix := ""
	if pct != nil {
		suffix = fmt.Sprintf(" %d%%", *pct)
	}
	return quotaFit(l.t(zh, en), suffix)
}

// quotaFit is label then suffix in at most segmentedTitleMost characters, cutting the label (never mid-rune) first so
// the suffix (a percent, a number) stays whole; a suffix too long on its own is cut too.
func quotaFit(label, suffix string) string {
	cut := func(s string, n int) string {
		if r := []rune(s); len(r) > n {
			return string(r[:max(0, n)])
		}
		return s
	}
	suffix = cut(suffix, segmentedTitleMost)
	out := strings.TrimSpace(cut(label, segmentedTitleMost-utf8.RuneCountInString(suffix)) + suffix)
	if out == "" {
		return "?"
	}
	return out
}

func quotaEstimatedSection(l lang, acct protocol.Account) wire.Section {
	if acct.Provider == "" || acct.Provider == echo.Name {
		return wire.Section{
			Title: l.t("额度", "Allowance"),
			Items: []wire.Component{{Type: "text", Style: "secondary",
				Text: l.t("这个服务还没告诉我们额度用了多少。", "The service has not reported any allowance yet.")}},
		}
	}
	sent5h, sent24h := 0, 0
	if acct.Usage != nil {
		sent5h, sent24h = acct.Usage.Sent5h, acct.Usage.Sent24h
	}
	usageVal := l.t("近 24 小时暂无发送记录", "No sends in the last 24 hours")
	if sent24h > 0 || sent5h > 0 {
		usageVal = l.t(fmt.Sprintf("近 5 小时已发 %d 条 · 近 24 小时已发 %d 条", sent5h, sent24h),
			fmt.Sprintf("%d sent in 5h · %d in 24h", sent5h, sent24h))
	}
	return wire.Section{
		Title: l.t("额度与用量", "Allowance & usage"),
		Items: []wire.Component{
			{
				Type:  "row",
				Title: l.t("现在状态", "Status"),
				Value: l.t("未触达限流", "No limit hit"),
				Badge: &wire.Badge{Text: l.t("估算", "Estimated"), Tone: wire.ToneNeutral},
			},
			{
				Type:  "row",
				Title: l.t("本地用量估算", "Local activity estimate"),
				Value: usageVal,
			},
			{
				Type:  "row",
				Title: l.t("重置时间", "Resets"),
				Value: l.t("未公开（触达限流时自动显示恢复时间）", "Not reported (shown automatically if rate limited)"),
			},
			{
				Type:     "row",
				Title:    l.t("来源与估算方式", "Source & estimation"),
				Value:    quotaEstimateSourceShort(l, acct.Provider),
				Subtitle: quotaEstimateMethod(l, acct.Provider),
			},
		},
	}
}

// quotaSummaryCompact is a short quota phrase for the home list row, or "" when nothing notable applies.
func quotaSummaryCompact(l lang, acct protocol.Account, c clock) string {
	if acct.Quota != nil {
		switch acct.Quota.State {
		case protocol.QuotaExhausted:
			if acct.Quota.ResetsAt > 0 {
				return l.t("额度用完了（"+c.at(l, acct.Quota.ResetsAt)+"恢复）", "used up (back "+c.at(l, acct.Quota.ResetsAt)+")")
			}
			return l.t("额度用完了", "allowance used up")
		case protocol.QuotaLow:
			if acct.Quota.Percent != nil {
				return l.t(fmt.Sprintf("额度快用完（剩 %d%%）", *acct.Quota.Percent), fmt.Sprintf("low allowance (%d%% left)", *acct.Quota.Percent))
			}
			return l.t("额度快用完", "low allowance")
		default:
			if acct.Quota.Percent != nil {
				return l.t(fmt.Sprintf("剩 %d%% 额度", *acct.Quota.Percent), fmt.Sprintf("%d%% left", *acct.Quota.Percent))
			}
		}
	}
	if acct.Usage != nil && acct.Usage.Sent24h > 0 && acct.Provider != echo.Name {
		return l.t(fmt.Sprintf("近 24 小时已发 %d 条（估算）", acct.Usage.Sent24h), fmt.Sprintf("%d sent in 24h (est.)", acct.Usage.Sent24h))
	}
	return ""
}

// quotaSummaryFull is the quota fragment for the 账号 management list row.
func quotaSummaryFull(l lang, acct protocol.Account, c clock) string {
	if acct.Quota != nil {
		switch acct.Quota.State {
		case protocol.QuotaExhausted:
			if acct.Quota.ResetsAt > 0 {
				return l.t("额度已用完，"+c.at(l, acct.Quota.ResetsAt)+"恢复", "used up, resets "+c.at(l, acct.Quota.ResetsAt))
			}
			return l.t("额度已用完", "allowance used up")
		case protocol.QuotaLow:
			s := l.t("额度快用完", "low allowance")
			if acct.Quota.Percent != nil {
				s = l.t(fmt.Sprintf("额度仅剩 %d%%", *acct.Quota.Percent), fmt.Sprintf("only %d%% left", *acct.Quota.Percent))
			}
			if acct.Quota.ResetsAt > 0 {
				s += l.t("（"+c.at(l, acct.Quota.ResetsAt)+"重置）", " (resets "+c.at(l, acct.Quota.ResetsAt)+")")
			}
			return s
		default:
			if acct.Quota.Percent != nil {
				s := l.t(fmt.Sprintf("剩 %d%% 额度", *acct.Quota.Percent), fmt.Sprintf("%d%% allowance left", *acct.Quota.Percent))
				if acct.Quota.ResetsAt > 0 {
					s += l.t("（"+c.at(l, acct.Quota.ResetsAt)+"重置）", " (resets "+c.at(l, acct.Quota.ResetsAt)+")")
				}
				return s
			}
		}
	}
	if acct.Provider == "" || acct.Provider == echo.Name {
		return ""
	}
	if acct.Usage != nil && acct.Usage.Sent24h > 0 {
		return l.t(fmt.Sprintf("用量估算：近 24 小时已发 %d 条", acct.Usage.Sent24h), fmt.Sprintf("est.: %d sent in 24h", acct.Usage.Sent24h))
	}
	return l.t("额度按发送记录估算", "allowance estimated locally")
}

func quotaResetWords(l lang, c clock, resetsAt int64) string {
	rel := c.at(l, resetsAt)
	if !c.relative(resetsAt) {
		return rel
	}
	loc := c.loc
	if loc == nil {
		loc = time.UTC
	}
	t := time.UnixMilli(resetsAt).In(loc)
	exact := l.t(t.Format("1月2日 15:04"), t.Format("Jan 2 15:04"))
	return rel + l.t("（"+exact+"）", " ("+exact+")")
}

func quotaBucketDetail(l lang, b protocol.QuotaBucket, c clock) string {
	var parts []string
	if b.State == protocol.QuotaExhausted {
		parts = append(parts, l.t("已用完", "Used up"))
	} else if b.State == protocol.QuotaLow {
		parts = append(parts, l.t("快用完", "Low"))
	}
	if b.Limit != nil && *b.Limit > 0 && b.Used != nil {
		left := math.Max(0, *b.Limit-*b.Used)
		parts = append(parts, fmt.Sprintf(l.t("剩余 %.0f / %.0f", "%.0f / %.0f left"), left, *b.Limit))
	}
	if b.ResetsAt > 0 {
		parts = append(parts, l.t(c.at(l, b.ResetsAt)+"重置", "resets "+c.at(l, b.ResetsAt)))
	}
	return strings.Join(parts, " · ")
}

func quotaSourceLabel(l lang, prov string, q *protocol.Quota) string {
	if q == nil {
		return ""
	}
	if q.Source == protocol.QuotaSourceResponse {
		return l.t("实时限流反馈（对方返回已达上限）", "Live rate-limit response from service")
	}
	switch q.SourceNote {
	case "grok_bot:GetSandUsageStatus", "grok_bot:GetCurrentPeriodUsage":
		return l.t("真实数据 · Grok Bot 官方用量", "Real data · Grok Bot official usage")
	case "claude_duet:organizations/usage":
		return l.t("真实数据 · Claude 网页端用量", "Real data · Claude web usage")
	case "dots:wham/usage":
		return l.t("真实数据 · ChatGPT 网页端用量", "Real data · ChatGPT web usage")
	case "muse:settings/usage":
		return l.t("真实数据 · Muse 网页端用量", "Real data · Muse web usage")
	case "gemini:usage":
		return l.t("真实数据 · Gemini 网页端用量", "Real data · Gemini web usage")
	case "groq:headers":
		return l.t("真实数据 · Groq 实时额度反馈", "Real data · Groq live allowance")
	case "cue:headers":
		return l.t("真实数据 · Manus 实时额度反馈", "Real data · Manus live allowance")
	}
	if prov == groq.Name {
		return l.t("真实数据 · Groq 实时额度反馈", "Real data · Groq live allowance")
	}
	return l.t("真实数据 · "+providerName(l, prov)+" 官方返回", "Real data · reported by "+providerName(l, prov))
}

func quotaEstimateSourceShort(l lang, prov string) string {
	switch prov {
	case grokBot, "claude_duet", "dots", providerMuse, "gemini_tasks", groq.Name:
		return l.t("等待读取真实额度（暂按本地记录估算）", "Waiting for first real allowance reading (local estimate meanwhile)")
	default:
		return l.t("本地发送记录估算（服务未公开实时额度）", "Local send-count estimate (service does not expose real-time allowance)")
	}
}

func quotaEstimateMethod(l lang, prov string) string {
	switch prov {
	case providerMuse:
		return l.t("支持从已登录的 muse.ai 设置页「使用情况」读取每周实时用量、重置时间与额外额度；点上面的「立即同步」即可拉取，拉取前先展示本地发送计数。",
			"Supports live weekly usage, reset time, and extra credits from signed-in muse.ai Settings › Usage; tap Sync now above to fetch.")
	case "gemini_tasks":
		return l.t("支持从已登录的 gemini.google.com/usage 读取当前与每周实时用量和重置时间；点上面的「立即同步」即可拉取，拉取前先展示本地发送计数。",
			"Supports live current and weekly usage with reset times from signed-in gemini.google.com/usage; tap Sync now above to fetch.")
	case cue.Name:
		return l.t("Manus 未提供余额查询；这里按本机记录的 5 小时/24 小时发送条数估算，收到对方限流提示时会自动显示真实额度。",
			"Manus offers no balance query; estimated from local 5h/24h sends, upgrading automatically when live rate-limit data arrives.")
	case muse2api.Name:
		return l.t("自建转接服务会在多个 Muse 账号间分摊请求，通常不返回剩余额度；这里按本机发送记录估算，遇到限流时会显示恢复时间。",
			"Your Muse relay spreads traffic across its own accounts without reporting remaining allowance; estimated from local sends and live cooldowns.")
	case grokBot:
		return l.t("支持读取 Grok Bot 官方实时用量和重置时间；点上面的「立即同步」即可拉取最新额度，拉取前先展示本地发送计数。",
			"Supports live Grok Bot usage and reset time; tap Sync now above to fetch the latest reading.")
	case "claude_duet":
		return l.t("支持从已登录的 claude.ai 读取 5 小时、每周与 Duet 专属实时额度；点上面的「立即同步」即可拉取，拉取前先展示本地发送计数。",
			"Supports live 5-hour, weekly, and Duet credit usage from signed-in claude.ai; tap Sync now above to fetch.")
	case "dots":
		return l.t("支持从已登录的 chatgpt.com 读取当前时段实时额度；点上面的「立即同步」即可拉取，拉取前先展示本地发送计数。",
			"Supports live rate-limit windows from signed-in chatgpt.com; tap Sync now above to fetch.")
	case groq.Name:
		return l.t("Groq 每次回复都会附带真实剩余次数和字数额度；发一条消息后会自动显示，之前先展示本地发送计数。",
			"Groq returns live remaining requests and tokens on every reply; shown automatically after your next send.")
	default:
		return l.t("按本机记录的 5 小时/24 小时发送条数估算；触发限流时会自动切为真实限流状态。",
			"Estimated from local 5h/24h sends; switches to live cooldown if rate limited.")
	}
}
