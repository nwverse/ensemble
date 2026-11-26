package extension

import (
	"context"
	"errors"
	"strings"

	"github.com/nwverse/persona/sdk/go/ext"
	"github.com/nwverse/persona/sdk/go/wire"

	"github.com/nwverse/ensemble/internal/core"
	"github.com/nwverse/ensemble/internal/protocol"
	"github.com/nwverse/ensemble/internal/provider"
	"github.com/nwverse/ensemble/internal/provider/browser"
)

// A provider's error code reaches a person in several places: the account list, the account page, a pairing that
// failed, a send that failed, and the assistant (which tells the user). problemOf is the one place that says it in
// words: what happened, and what to do about it.

// place is where an account lives, for the words: its provider, and for a browser-driven one the computer it runs
// on (its display name, never its id) and the browser backend there.
type place struct {
	Provider string
	Browser  bool
	Machine  string // the computer's display name, "" when not known
	Backend  string
	Offline  bool // the computer is known to be offline now
	Local    bool // the computer is this one (the standalone driver's own browser)
	// Pairing is the pairing button the words send the user back to, when they are for a pairing that failed
	// (nothing is paired yet): 「配对」 on the key page, 「验证并配对」 on the browser one. "" for a paired account.
	Pairing string
}

// problem is an account's last error in words.
type problem struct {
	Code  string
	Short string // a few words for a list row
	What  string // what happened
	Do    string // what to do
	// NeedsUser: it does not pass by waiting; the user has to do something.
	NeedsUser bool
	// Recheck: once the user did it, 「重新检查」 (a sync now) tells whether it is fixed.
	Recheck bool
}

// Text is the whole of it: what happened, then what to do.
func (p problem) Text(l lang) string { return p.What + l.t("", " ") + p.Do }

// siteFacts are what the words name for a browser-driven site: who checks, the address to open, and the check.
type siteFacts struct{ gate, host, check string }

func siteOf(l lang, p string) siteFacts {
	switch p {
	case "gemini_tasks":
		return siteFacts{"Google", "gemini.google.com", l.t("「我不是机器人」验证", "the \"I'm not a robot\" check")}
	case "dots":
		return siteFacts{"ChatGPT", "chatgpt.com", l.t("「确认你是真人」验证", "the \"Verify you are human\" check")}
	case "claude_duet":
		return siteFacts{"Claude", "claude.ai", l.t("「确认你是真人」验证", "the \"Verify you are human\" check")}
	case "muse":
		return siteFacts{"Muse", "muse.ai", l.t("人机验证", "the human check")}
	}
	return siteFacts{providerName(l, p), "", l.t("人机验证", "the human check")}
}

// problemOf says code in words for an account at w; ok is false for no error.
func problemOf(l lang, code string, w place) (problem, bool) {
	if code == "" {
		return problem{}, false
	}
	label := providerName(l, w.Provider)
	site := siteOf(l, w.Provider)
	pairing := w.Pairing != ""
	machine := l.t("「"+w.Machine+"」", "\""+w.Machine+"\"")
	switch {
	case w.Local:
		machine = l.t("这台电脑", "this computer")
	case w.Machine == "" && pairing:
		machine = l.t("刚才选的那台电脑", "the computer you chose")
	case w.Machine == "":
		machine = l.t("配对这个账号时选的那台电脑", "the computer chosen when this account was paired")
	}
	// The browser the site is driven in, said in plain words (the product's name once, in parentheses): Persona's
	// runs share that browser's sign-ins and cookies with the user's own tabs in it, so what the user does there is
	// what the runs see. Another browser (Chrome) keeps its own.
	browserName := l.t("Persona 控制的那个浏览器", "the browser Persona controls")
	browserFirst := browserName
	// egoOnly warns off another browser: ego lite keeps its own sign-ins, so a check passed in Chrome does not count
	// there. Said only for ego lite: with another backend the borrowed browser may be Chrome itself.
	egoOnly := ""
	if w.Backend == "" || w.Backend == "ego" {
		browserFirst = l.t(browserName+"（ego lite）", browserName+" (ego lite)")
		egoOnly = l.t("要在这个浏览器里做，在 Chrome 等你自己的浏览器里验证可能不管用。",
			" Do it in that browser: passing the check in your own browser such as Chrome may not count.")
	}
	// that is the computer again, in a later sentence.
	that := l.t("那台电脑", "that computer")
	if w.Local {
		that = l.t("这台电脑", "this computer")
	}
	recheck := l.t("「重新检查」", "\"Check again\"")
	switch w.Pairing {
	case pairKey:
		recheck = l.t("「添加」", "\"Add\"")
	case pairBrowser:
		recheck = l.t("「检查登录并添加」", "\"Check sign-in and add\"")
	}
	// later is what happens next by itself: a paired account is tried again on its own, a pairing is not.
	later := l.t("不用处理，稍后会自动再试", "nothing to do; it is tried again later on its own")
	blocked := l.t("所以现在读不到、也发不出", "so nothing can be read or sent")
	if pairing {
		later = l.t("过一会儿再点"+recheck+"试一次", "tap "+recheck+" again in a while")
		blocked = l.t("所以没能配对", "so it could not be paired")
	}
	p := problem{Code: code}
	switch {
	case code == provider.CodeHumanCheck:
		p.NeedsUser, p.Recheck = true, true
		p.Short = l.t("被 "+site.gate+" 的人机验证拦住了", "stopped by "+site.gate+"'s human check")
		p.What = l.t(site.gate+" 怀疑是机器人在访问，暂时拦住了 "+label+"，"+blocked+"。",
			site.gate+" suspects a robot is visiting and is holding "+label+" back for now, "+blocked+".")
		p.Do = l.t("请到"+machine+"上，打开 "+browserFirst+"，在里面打开 "+site.host+"，按页面提示完成"+site.check+"；做完回来点"+recheck+"。"+egoOnly,
			"Go to "+machine+", open "+browserFirst+", open "+site.host+" in it and complete "+site.check+" as the page asks; then come back and tap "+recheck+"."+egoOnly)
	case code == provider.CodeAuthFailed && w.Browser:
		p.NeedsUser, p.Recheck = true, true
		p.Short = l.t("那台电脑上没有登录", "signed out on that computer")
		p.What = l.t(label+" 在"+machine+"上没有登录，或者登录已经过期。",
			label+" is not signed in on "+machine+", or its sign-in expired.")
		p.Do = l.t("请到"+machine+"上，在 "+browserFirst+"里打开 "+site.host+" 重新登录；登好后点"+recheck+"。",
			"Go to "+machine+", open "+site.host+" in "+browserFirst+" and sign in again; then tap "+recheck+".")
	case code == provider.CodeAuthFailed && w.Provider == grokBot && !pairing:
		p.NeedsUser = true
		p.Short = l.t("登录失效", "signed out")
		p.What = l.t("Grok Bot 的登录已经失效。", "Grok Bot's sign-in is no longer valid.")
		p.Do = l.t("请在这个账号页点「重新登录」。", "Tap \"Sign in again\" on this account's page.")
	case code == provider.CodeAuthFailed:
		p.NeedsUser = true
		p.Short = l.t("密钥失效了", "the key no longer works")
		p.What = l.t(label+" 不认这个密钥（可能填错、过期或被撤销）。", label+" does not accept this key (mistyped, expired or revoked).")
		p.Do = l.t("到对方网站复制一个新的密钥，把这个账号移除后重新添加。", "Copy a new key from the service, then remove this account and add it again.")
		if pairing {
			p.Do = l.t("请检查后重新填写，再点"+recheck+"。", "Check it, enter it again and tap "+recheck+".")
		}
	case code == provider.CodeNetworkError && w.Browser:
		p.Recheck = true
		p.Short = l.t("连不上那台电脑", "cannot reach that computer")
		p.What = l.t("连不上"+machine+"，没法通过它的浏览器访问 "+label+"。", "Cannot reach "+machine+", so "+label+" cannot be reached through its browser.")
		if w.Offline {
			p.Short = l.t("那台电脑不在线", "that computer is offline")
			p.What = l.t(machine+"现在不在线，没法通过它的浏览器访问 "+label+"。", machine+" is offline, so "+label+" cannot be reached through its browser.")
		}
		p.Do = l.t("请确认"+that+"开着、联着网，Persona 在上面运行；连上后会自动再试，也可以点"+recheck+"。",
			"Make sure that computer is on, online and running Persona; it is tried again on its own once it is back, or tap "+recheck+".")
		if pairing {
			p.Do = l.t("请确认"+that+"开着、联着网，Persona 在上面运行，然后再点"+recheck+"。",
				"Make sure that computer is on, online and running Persona, then tap "+recheck+" again.")
		}
		if w.Local && w.Offline {
			// The standalone driver's own computer is never "offline": what is missing is the browser program.
			p.NeedsUser = true
			p.Short = l.t("这台电脑上没有可用的浏览器", "no browser to borrow on this computer")
			p.What = l.t("这台电脑上用不了 "+browserFirst+"，所以访问不了 "+label+"。",
				browserFirst+" is not available on this computer, so "+label+" cannot be reached.")
			p.Do = l.t("请在这台电脑上装好并打开它，然后点"+recheck+"。", "Install and open it on this computer, then tap "+recheck+".")
		}
	case code == provider.CodeNetworkError:
		p.Short = l.t("连不上服务", "cannot reach the service")
		p.What = l.t("连不上 "+label+" 的服务器。", "Cannot reach "+label+"'s servers.")
		p.Do = l.t("多半是网络波动，"+strings.TrimPrefix(later, "不用处理，")+"。", "Most likely a network hiccup; "+later+".")
	case code == provider.CodeRateLimited:
		p.Short = l.t("对方限流", "rate limited")
		p.What = l.t(label+" 说请求太多，暂时限流了。", label+" says there were too many requests and is limiting them for now.")
		p.Do = l.t(later+"。", later+".")
	case code == provider.CodeNotFound:
		p.Short = l.t("找不到", "not found")
		p.What = l.t(label+" 上找不到要找的内容（可能已删除，或这个账号下还没有）。", label+" has nothing at that address (deleted, or not made yet).")
		p.Do = l.t("确认它在 "+label+" 里还在；没问题就不用处理。", "Check it is still there in "+label+"; otherwise nothing to do.")
	case code == provider.CodeUpstreamError || code == provider.CodeBadResponse || code == provider.CodeUnavailable ||
		code == provider.CodeRequestFailed || code == "SYNC_FAILED":
		p.Recheck = w.Browser
		p.Short = l.t("对方出错", "the service failed")
		p.What = l.t(label+" 那边出错了，或者页面没有按预期回答。", label+" failed, or its page did not answer as expected.")
		p.Do = l.t("稍后会自动再试；一直这样的话点"+recheck+"看看。", "It is tried again later on its own; if it keeps happening, tap "+recheck+".")
		if !w.Browser || pairing {
			p.Do = l.t(later+"。", later+".")
		}
	default:
		// An unknown code stays out of the words (it is in Code, and in the logs).
		p.Short = l.t("出了点问题", "something went wrong")
		p.What = l.t(label+" 那边出了点问题。", label+" ran into a problem.")
		p.Do = l.t(later+"。", later+".")
	}
	if p.Do != "" && p.Do[0] >= 'a' && p.Do[0] <= 'z' {
		p.Do = strings.ToUpper(p.Do[:1]) + p.Do[1:] // an English sentence built from a clause
	}
	return p, true
}

// problemError is a failure already said in words (reason answers its text).
type problemError struct {
	p    problem
	text string
}

func (e *problemError) Error() string { return e.text }

// explain turns err into its words when it carries a provider's code (a pairing or a send the provider refused),
// and leaves it as it is otherwise.
func explain(l lang, err error, w place) error {
	code := protocol.ReasonOf(err)
	if code == "" || !providerCode(code) {
		return err
	}
	p, _ := problemOf(l, code, w)
	return &problemError{p: p, text: p.Text(l)}
}

func providerCode(code string) bool {
	switch code {
	case provider.CodeAuthFailed, provider.CodeRateLimited, provider.CodeNotFound, provider.CodeUnavailable,
		provider.CodeBadResponse, provider.CodeNetworkError, provider.CodeUpstreamError, provider.CodeRequestFailed,
		provider.CodeHumanCheck:
		return true
	}
	return false
}

// asProblem is err's words, when it has them.
func asProblem(err error) (*problemError, bool) {
	var pe *problemError
	return pe, errors.As(err, &pe)
}

// placer finds where accounts live for one answer: the computers are asked for (browser.machines, a call to
// another computer) at most once, and only for an account that has an error to explain.
type placer struct {
	space    *core.Space
	drv      browser.Driver
	machines []browser.Machine
	asked    bool
}

func newPlacer(space *core.Space, drv browser.Driver) *placer { return &placer{space: space, drv: drv} }

type placerKey struct{}

// placerFor is the placer an action put in ctx for the view it draws next (so the computers are asked once for
// both), or a new one.
func placerFor(ctx context.Context, space *core.Space, drv browser.Driver) *placer {
	if pl, ok := ctx.Value(placerKey{}).(*placer); ok && pl.space == space {
		return pl
	}
	return newPlacer(space, drv)
}

// of is where acct lives, to say code in words; for no code the words are not needed, and nothing is looked up.
func (pl *placer) of(ctx context.Context, acct protocol.Account, code string) place {
	w := basicPlace(acct)
	if !w.Browser || code == "" {
		return w
	}
	id, backend := pl.space.AccountMachine(ctx, acct.AccountID)
	w.Backend = backend
	if id != "" {
		pl.name(ctx, &w, id)
	}
	return w
}

// name fills in computer id's display name ("" when it has none or is not listed: its id is never shown), whether
// it is offline or this computer, and its backend.
func (pl *placer) name(ctx context.Context, w *place, id string) {
	if pl.drv == nil {
		return
	}
	if pl.drv.Name() == "ego" || id == "local" {
		// The standalone driver's own browser: the computer club runs on.
		w.Local = true
	}
	if !pl.asked {
		pl.asked = true
		pl.machines, _ = pl.drv.Machines(ctx)
	}
	for _, m := range pl.machines {
		if m.MachineID != id {
			continue
		}
		if w.Backend == "" {
			w.Backend = m.Backend
		}
		if m.Name != id {
			w.Machine = m.Name
		}
		w.Offline = !m.Online
		return
	}
}

// basicPlace is where acct lives without looking its computer up (a list row's few words do not name it).
func basicPlace(acct protocol.Account) place {
	_, ok := browserProviderLabel(acct.Provider)
	return place{Provider: acct.Provider, Browser: ok}
}

// The pairing pages a failed pairing's words send the user back to, by their button: 「添加」 on the key page
// (pairView), 「检查登录并添加」 on the browser one (browserPairView).
const (
	pairKey     = "key"
	pairBrowser = "browser"
)

// pairPlace is where a browser pairing on rawChoice (the machine field) would have lived.
func (a *accounts) pairPlace(ctx context.Context, providerName, rawChoice string) place {
	cred := parseMachineChoice(rawChoice)
	w := place{Provider: providerName, Browser: true, Pairing: pairBrowser, Backend: cred.Backend}
	if cred.MachineID != "" {
		newPlacer(nil, a.browser).name(ctx, &w, cred.MachineID)
	}
	return w
}

// recheck is 「重新检查」: a sync now, then the account as it is after it, in words.
func (a *accounts) recheck(ctx context.Context, r *ext.UIRequest, l lang, st pageState) (*wire.ActionResult, error) {
	answer := func(note, tone string) (*wire.ActionResult, error) {
		st.Note, st.NoteTone = note, tone
		return &wire.ActionResult{View: a.view(ctx, r, st), Toast: note}, nil
	}
	space, err := a.club.Space(ctx, core.DefaultSpace)
	if err != nil {
		return answer(reason(l, err, ""), wire.ToneDanger)
	}
	pl := newPlacer(space, a.browser)
	ctx = context.WithValue(ctx, placerKey{}, pl)
	acct, err := space.Account(ctx, st.Account)
	if err != nil {
		return answer(reason(l, err, ""), wire.ToneDanger)
	}
	if acct.Status == protocol.AccountPaused {
		// A paused account is not synced: nothing was checked.
		return answer(l.t("这个账号已暂停同步，没有检查：先在下面点「恢复同步」。", "This account is paused, so nothing was checked: resume it first."), wire.ToneCaution)
	}
	// A sync asked for while one is running joins it, and that one may have started before the user solved the
	// problem: one that still fails is followed by a fresh one, which starts after the user's tap.
	for try := 0; try < 2; try++ {
		if err := call(ctx, a.club, protocol.MethodClubAccountsSync, protocol.AccountParams{AccountID: st.Account}, nil); err != nil {
			return answer(reason(l, err, ""), wire.ToneDanger)
		}
		if acct, err = space.Account(ctx, st.Account); err != nil {
			return answer(reason(l, err, ""), wire.ToneDanger)
		}
		if acct.LastError == "" {
			break
		}
	}
	p, ok := problemOf(l, acct.LastError, pl.of(ctx, acct, acct.LastError))
	if !ok {
		return answer(l.t("检查过了，已经恢复正常，最新的对话也拉下来了", "Checked: working again, and the latest chats are in"), wire.TonePositive)
	}
	return answer(l.t("检查过了，还没好。", "Checked; still not working. ")+p.Text(l), wire.ToneCaution)
}
