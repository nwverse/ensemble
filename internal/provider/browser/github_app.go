package browser

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/nwverse/ensemble/internal/provider"
)

// Adding repositories to a GitHub App's grant on an owner (the Claude GitHub App, which Claude Duet sees
// repositories through), in the owner's own signed-in browser. Seen on github.com (2026-11-27):
//
//   - https://github.com/apps/<app>/installations/new answers with select_target: one row per account the app is
//     installed on, "Configure <owner>", linking to its installation's settings page: /settings/installations/<id>
//     for the person's own account, /organizations/<org>/settings/installations/<id> or
//     /apps/<app>/installations/<id> for an organization.
//   - An installation's settings page is sudo-protected: GitHub first shows "Confirm access" (passkey, GitHub
//     Mobile, an authenticator code or an emailed code; form action /sessions/sudo). Ensemble never answers it.
//   - Past it, the page's form has install_target radios (all | selected) and one hidden repository_ids[] per
//     selected repository; Save submits them. An organization member who is not an owner gets a request instead
//     of Save, which an owner approves.
//
// The page is only changed when everything holds: signed in, no confirm-access or human check, an installation
// whose grant is "selected", each repository found. Then the repository ids are added to the form's own (none
// taken out, the radio untouched) and the form saved, and the settings page read again. Anything else stops with
// the page's address for the owner to finish there, and nothing changed.

// gitHubApp is a GitHub App an agent reads repositories through.
type gitHubApp struct{ slug string }

var claudeGitHubApp = gitHubApp{slug: "claude"}

func (g gitHubApp) installURL() string {
	return "https://github.com/apps/" + g.slug + "/installations/new"
}

// The pages' selectors (testdata/github: select_target and the confirm-access page recorded, the settings form
// reconstructed, as the confirm-access page stands in front of it).
const (
	ghSudoSel      = `form[action*="/sessions/sudo"]`
	ghCaptchaSel   = `iframe[src*="captcha"], [class*="octocaptcha"]`
	ghTargetRowSel = `a[href*="/installations/"]`
	ghRowOwnerSel  = `.text-bold`
	ghTargetSel    = `input[name="install_target"]`
	ghRepoIDSel    = `input[name^="repository_ids"]`
	ghSubmitSel    = `button[type="submit"], input[type="submit"]`
	ghSlot         = "pac-github"
	ghTimeout      = 60 * time.Second
)

// ghFacts is what a page shows, as the probe reads it (no decision in the page: ghState decides).
type ghFacts struct {
	URL      string   `json:"url"`
	SignedIn bool     `json:"signed_in"`
	Sudo     bool     `json:"sudo"`
	Captcha  bool     `json:"captcha"`
	Targets  []string `json:"targets"` // install_target values
	Target   string   `json:"target"`  // the checked one
	IDs      []string `json:"ids"`     // repository_ids in the form
	Submits  []string `json:"submits"` // the form's submit buttons' words
	Request  bool     `json:"request"` // the page speaks of a request to the owners
}

// ghProbeJS reads ghFacts in the page (defined in page.evaluate's callback, SEL its selectors).
const ghProbeJS = `
const __facts = (SEL) => {
  const q = s => document.querySelector(s);
  const targets = [...document.querySelectorAll(SEL.target)];
  const checked = targets.find(t => t.checked);
  const form = (checked || targets[0] || {}).form || null;
  const submits = form ? [...form.querySelectorAll(SEL.submit)].map(b => String(b.innerText || b.value || "").replace(/\s+/g, " ").trim()) : [];
  return {
    url: String(location.href),
    signed_in: !/^\/(login|session|sessions\/(two-factor|verified-device)|orgs\/[^/]+\/sso)(\/|$)/.test(location.pathname) &&
      !!String((document.querySelector('meta[name="user-login"]') || {}).content || "x").trim(),
    sudo: !!q(SEL.sudo),
    captcha: !!q(SEL.captcha),
    targets: targets.map(t => String(t.value)),
    target: checked ? String(checked.value) : "",
    ids: form ? [...form.querySelectorAll(SEL.ids)].filter(i => !i.disabled && (i.type === "hidden" || i.checked)).map(i => String(i.value)).filter(Boolean) : [],
    submits,
    request: /\brequest\b/i.test(String((document.querySelector("main") || document.body || {}).innerText || "")),
  };
};
`

func ghSelectors() map[string]string {
	return map[string]string{"sudo": ghSudoSel, "captcha": ghCaptchaSel, "rows": ghTargetRowSel, "owner": ghRowOwnerSel,
		"target": ghTargetSel, "ids": ghRepoIDSel, "submit": ghSubmitSel}
}

// ghProbeScript opens the app's install page, follows the owner's Configure row to the installation's settings
// page, reads it, and looks up each repository's id on its own page. ARGS: {install, owner, repos, sel}.
const ghProbeScript = `
const SEL = ARGS.sel;
await page.goto(ARGS.install);
await page.waitForLoadState("domcontentloaded").catch(() => {});
const pick = await page.evaluate((A) => {
` + ghProbeJS + `
  const f = __facts(A.sel);
  if (f.sudo || f.captcha || !f.signed_in) return { facts: f };
  for (const a of document.querySelectorAll(A.sel.rows)) {
    const who = String(((a.querySelector(A.sel.owner) || {}).textContent) || "").trim();
    if (who.toLowerCase() !== A.owner.toLowerCase()) continue;
    return { facts: f, href: a.href, configure: /configure/i.test(String(a.textContent || "")) };
  }
  return { facts: f, none: true };
}, ARGS);
if (!pick.href || !pick.configure) return { install: pick.facts, configure_url: "", none: true };
await page.goto(pick.href);
await page.waitForLoadState("domcontentloaded").catch(() => {});
return await page.evaluate(async (A) => {
` + ghProbeJS + `
  const f = __facts(A.sel);
  const out = { install: null, configure_url: A.href, facts: f, lookup: {}, missing: [] };
  if (f.sudo || f.captcha || !f.signed_in || f.target !== "selected") return out;
  for (const name of A.repos) {
    try {
      const r = await fetch("/" + encodeURIComponent(A.owner) + "/" + encodeURIComponent(name), { credentials: "include" });
      if (!r.ok) { out.missing.push(name); continue; }
      const doc = new DOMParser().parseFromString(await r.text(), "text/html");
      const id = (doc.querySelector('meta[name="octolytics-dimension-repository_id"]') || {}).content || "";
      if (/^[0-9]+$/.test(id)) out.lookup[name] = id; else out.missing.push(name);
    } catch (e) { out.missing.push(name); }
  }
  return out;
}, { ...ARGS, href: pick.href });
`

// ghSaveScript opens the settings page again, checks it still is what the probe saw (the grant "selected", the
// same repositories selected), adds the ids to the form, saves, and reads the page again. ARGS: {url, before, add,
// sel}.
const ghSaveScript = `
await page.goto(ARGS.url);
await page.waitForLoadState("domcontentloaded").catch(() => {});
const done = await page.evaluate((A) => {
` + ghProbeJS + `
  const f = __facts(A.sel);
  if (f.sudo || f.captcha || !f.signed_in) return { stop: f };
  const same = f.target === "selected" && f.ids.length === A.before.length && A.before.every(id => f.ids.includes(id));
  if (!same) return { changed: f };
  const target = [...document.querySelectorAll(A.sel.target)].find(t => t.checked);
  const form = target.form;
  for (const id of A.add) {
    if (f.ids.includes(id)) continue;
    const i = document.createElement("input");
    i.type = "hidden"; i.name = "repository_ids[]"; i.value = id;
    form.appendChild(i);
  }
  const now = [...form.querySelectorAll(A.sel.ids)].filter(i => !i.disabled && (i.type === "hidden" || i.checked)).map(i => String(i.value));
  const want = [...new Set([...A.before, ...A.add])];
  if (now.length !== want.length || want.some(id => !now.includes(id)) || !target.checked) return { merge: now };
  const save = [...form.querySelectorAll(A.sel.submit)].find(b => /^save$/i.test(String(b.innerText || b.value || "").trim()));
  if (!save) return { nosave: f };
  window.__pacSaving = true; // gone once the save's response replaces this document
  setTimeout(() => form.requestSubmit(save), 0);
  return { submitted: true };
}, ARGS);
if (!done.submitted) return done;
// The form's PUT loads a new document (the settings page again, or another): wait for it rather than cutting the
// save short with a goto.
await page.waitForFunction(() => !window.__pacSaving, undefined, { timeout: 20000 }).catch(() => {});
await page.waitForLoadState("domcontentloaded").catch(() => {});
await new Promise(r => setTimeout(r, 1500));
await page.goto(ARGS.url);
await page.waitForLoadState("domcontentloaded").catch(() => {});
return { submitted: true, after: await page.evaluate((A) => {
` + ghProbeJS + `
  return __facts(A.sel);
}, ARGS) };
`

type ghProbeAnswer struct {
	Install      *ghFacts          `json:"install"`
	ConfigureURL string            `json:"configure_url"`
	None         bool              `json:"none"`
	Facts        *ghFacts          `json:"facts"`
	Lookup       map[string]string `json:"lookup"`
	Missing      []string          `json:"missing"`
}

type ghSaveAnswer struct {
	Error     string   `json:"error"`
	Stop      *ghFacts `json:"stop"`
	Changed   *ghFacts `json:"changed"`
	Merge     []string `json:"merge"`
	NoSave    *ghFacts `json:"nosave"`
	Submitted bool     `json:"submitted"`
	After     *ghFacts `json:"after"`
}

// ghStop is the stop a page's facts call for ("" when the page may be used).
func ghStop(f ghFacts) provider.AuthorizeState {
	switch {
	case !f.SignedIn:
		return provider.AuthorizeSignIn
	case f.Sudo:
		return provider.AuthorizeSudo
	case f.Captcha:
		return provider.AuthorizeHumanCheck
	}
	return ""
}

// ghSettingsState is what an installation's settings page allows: "" to go on (a grant of selected repositories
// that the owner saves), else the stop.
func ghSettingsState(f ghFacts) (provider.AuthorizeState, bool) {
	if s := ghStop(f); s != "" {
		return s, false
	}
	if f.Target == "all" {
		return provider.AuthorizeAlready, true
	}
	if f.Request {
		// The page speaks of a request to the owners: never saved by Ensemble (a guide at worst).
		return provider.AuthorizeApproval, false
	}
	if f.Target != "selected" || !slices.Contains(f.Targets, "all") {
		if f.Request || slices.ContainsFunc(f.Submits, isRequestWord) {
			return provider.AuthorizeApproval, false
		}
		return provider.AuthorizeUnrecognized, false
	}
	if !slices.ContainsFunc(f.Submits, func(s string) bool { return strings.EqualFold(s, "save") }) {
		if slices.ContainsFunc(f.Submits, isRequestWord) || f.Request {
			return provider.AuthorizeApproval, false
		}
		return provider.AuthorizeUnrecognized, false
	}
	return "", false
}

var requestWord = regexp.MustCompile(`(?i)\brequest\b`)

func isRequestWord(s string) bool { return requestWord.MatchString(s) }

// authorizeGitHubApp adds repos of owner to app's grant on owner (provider.RepoMounter.AuthorizeRepos).
func authorizeGitHubApp(ctx context.Context, a *Adapter, credential string, app gitHubApp, owner string, repos []string) (provider.AuthorizeResult, error) {
	res := provider.AuthorizeResult{Owner: owner, URL: app.installURL(), Repos: []string{}}
	var names []string
	for _, r := range repos {
		c, ok := provider.CanonicalRepo(r)
		if !ok {
			return res, &provider.Error{Code: provider.CodeBadResponse}
		}
		o, n := provider.RepoOwnerName(c)
		if !strings.EqualFold(o, owner) {
			return res, &provider.Error{Code: provider.CodeBadResponse}
		}
		if !slices.ContainsFunc(names, func(x string) bool { return strings.EqualFold(x, n) }) {
			names = append(names, n)
			res.Repos = append(res.Repos, c)
		}
	}
	if len(names) == 0 {
		res.State = provider.AuthorizeAlready
		return res, nil
	}
	var probe ghProbeAnswer
	if err := ghRun(ctx, a, credential, ghProbeScript, map[string]any{"install": app.installURL(), "owner": owner,
		"repos": names, "sel": ghSelectors()}, &probe); err != nil {
		return res, err
	}
	return ghAfterProbe(res, probe, names, func(url string, before, add []string) (ghSaveAnswer, error) {
		var save ghSaveAnswer
		err := ghRun(ctx, a, credential, ghSaveScript, map[string]any{"url": url, "before": before, "add": add,
			"sel": ghSelectors()}, &save)
		return save, err
	})
}

// ghAfterProbe decides from the probe and, only when the page may be used, saves through save; then checks what the
// page shows afterwards.
func ghAfterProbe(res provider.AuthorizeResult, probe ghProbeAnswer, names []string,
	save func(url string, before, add []string) (ghSaveAnswer, error)) (provider.AuthorizeResult, error) {
	if probe.Install != nil {
		if s := ghStop(*probe.Install); s != "" {
			res.State = s
			return res, nil
		}
	}
	if probe.ConfigureURL == "" || probe.Facts == nil {
		res.State = provider.AuthorizeNotInstalled
		return res, nil
	}
	res.URL = probe.ConfigureURL
	state, all := ghSettingsState(*probe.Facts)
	if state != "" {
		res.State, res.AllRepos = state, all
		return res, nil
	}
	var missing []string
	var add []string
	for _, n := range names {
		id := probe.Lookup[n]
		if id == "" || slices.ContainsFunc(probe.Missing, func(m string) bool { return strings.EqualFold(m, n) }) {
			missing = append(missing, "https://github.com/"+res.Owner+"/"+n)
			continue
		}
		if !slices.Contains(probe.Facts.IDs, id) {
			add = append(add, id)
		}
	}
	if len(missing) > 0 {
		res.State, res.Missing = provider.AuthorizeNotFound, missing
		return res, nil
	}
	if len(add) == 0 {
		res.State = provider.AuthorizeAlready
		return res, nil
	}
	before := slices.Clone(probe.Facts.IDs)
	saved, err := save(probe.ConfigureURL, before, add)
	if err != nil {
		return res, err
	}
	switch {
	case saved.Stop != nil:
		if s := ghStop(*saved.Stop); s != "" {
			res.State = s
			return res, nil
		}
		res.State = provider.AuthorizeUnrecognized
		return res, nil
	case saved.Changed != nil, saved.Merge != nil, saved.NoSave != nil, !saved.Submitted:
		// The page was not what the probe saw, or the merge did not come out exact: nothing was saved.
		res.State = provider.AuthorizeUnrecognized
		return res, nil
	}
	if saved.After == nil {
		res.State = provider.AuthorizeUnconfirmed
		return res, nil
	}
	if s := ghStop(*saved.After); s != "" {
		res.State = provider.AuthorizeUnconfirmed
		return res, nil
	}
	if !ghSavedExactly(*saved.After, before, add) {
		res.State = provider.AuthorizeUnconfirmed
		return res, nil
	}
	res.State = provider.AuthorizeDone
	for n, id := range probe.Lookup {
		if slices.Contains(add, id) {
			res.Added = append(res.Added, "https://github.com/"+res.Owner+"/"+n)
		}
	}
	slices.Sort(res.Added)
	return res, nil
}

// ghSavedExactly says the settings page afterwards shows the grant still "selected", every repository it had, and
// each one added.
func ghSavedExactly(after ghFacts, before, add []string) bool {
	if after.Target != "selected" {
		return false
	}
	for _, id := range append(slices.Clone(before), add...) {
		if !slices.Contains(after.IDs, id) {
			return false
		}
	}
	return true
}

// ghLocks serializes GitHub runs per computer: they share the pac-github tab, and one run's goto must not cut
// another's save short.
var ghLocks sync.Map // machine id → *sync.Mutex

// ghRun runs a GitHub script in the computer's browser, in its own task space (not the agent's site's).
func ghRun(ctx context.Context, a *Adapter, credential, body string, args any, out any) error {
	if a.driver == nil {
		return &provider.Error{Code: provider.CodeUpstreamError}
	}
	cred, err := ParseCredential(credential)
	if err != nil {
		return err
	}
	mu, _ := ghLocks.LoadOrStore(cred.MachineID+"\x00"+cred.Backend, &sync.Mutex{})
	mu.(*sync.Mutex).Lock()
	defer mu.(*sync.Mutex).Unlock()
	argv, _ := json.Marshal(args)
	res, err := a.driver.Run(ctx, RunRequest{MachineID: cred.MachineID, Backend: cred.Backend, Slot: ghSlot,
		Script: fmt.Sprintf("const ARGS = %s;\n%s", argv, body), TimeoutMS: int(ghTimeout / time.Millisecond)})
	if err != nil {
		return &provider.Error{Code: provider.CodeNetworkError, Retryable: true}
	}
	if strings.TrimSpace(res.Error) != "" {
		return &provider.Error{Code: provider.CodeUpstreamError, Retryable: true}
	}
	payload := scriptValue(res.Output)
	if payload == "" || json.Unmarshal([]byte(payload), out) != nil {
		return &provider.Error{Code: provider.CodeUpstreamError, Retryable: true}
	}
	return nil
}
