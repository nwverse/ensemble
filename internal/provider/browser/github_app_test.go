package browser

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"golang.org/x/net/html"

	"github.com/nwverse/ensemble/internal/provider"
)

// testdata/github:
//
//	select-target.html     recorded (2026-11-27, read only): github.com/apps/claude/installations/new, the app on a
//	                       person's account and on an organization, each row "Configure"
//	sudo.html              recorded (2026-11-27, read only): an installation's settings page answered with "Confirm
//	                       access" (passkey, GitHub Mobile, authenticator, email code)
//	settings-selected.html reconstructed: the settings form past it, "Only select repositories", two selected
//	settings-all.html      the same with "All repositories"
//	settings-request.html  reconstructed: a member who is not an owner, Request instead of Save
//
// Names, ids, tokens and the masked email are replaced; scripts, styles and pictures left out.

func loadGitHubPage(t *testing.T, name string) *html.Node {
	t.Helper()
	f, err := os.Open(filepath.Join("testdata", "github", name))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	doc, err := html.Parse(f)
	if err != nil {
		t.Fatal(err)
	}
	return doc
}

// factsOf reads a page as ghProbeJS's __facts does, with the same selectors.
func factsOf(doc *html.Node, path string) ghFacts {
	f := ghFacts{URL: "https://github.com" + path, Targets: []string{}, IDs: []string{}, Submits: []string{},
		SignedIn: !regexp.MustCompile(`^/(login|session|sessions/(two-factor|verified-device)|orgs/[^/]+/sso)(/|$)`).MatchString(path),
		Sudo:     len(querySelectorAll(doc, ghSudoSel)) > 0, Captcha: len(querySelectorAll(doc, ghCaptchaSel)) > 0}
	targets := querySelectorAll(doc, ghTargetSel)
	var checked *html.Node
	for _, t := range targets {
		f.Targets = append(f.Targets, attr(t, "value"))
		if _, ok := attrOK(t, "checked"); ok && checked == nil {
			checked = t
		}
	}
	formOf := func(n *html.Node) *html.Node {
		for p := n; p != nil; p = p.Parent {
			if p.Type == html.ElementNode && p.Data == "form" {
				return p
			}
		}
		return nil
	}
	var form *html.Node
	if checked != nil {
		f.Target = attr(checked, "value")
		form = formOf(checked)
	} else if len(targets) > 0 {
		form = formOf(targets[0])
	}
	if form != nil {
		for _, b := range querySelectorAll(form, ghSubmitSel) {
			f.Submits = append(f.Submits, strings.Join(strings.Fields(nodeText(b)), " "))
		}
		for _, i := range querySelectorAll(form, ghRepoIDSel) {
			_, checked := attrOK(i, "checked")
			_, disabled := attrOK(i, "disabled")
			if v := attr(i, "value"); v != "" && !disabled && (attr(i, "type") == "hidden" || checked) {
				f.IDs = append(f.IDs, v)
			}
		}
	}
	main := querySelectorAll(doc, "main")
	text := nodeText(doc)
	if len(main) > 0 {
		text = nodeText(main[0])
	}
	f.Request = regexp.MustCompile(`(?i)\brequest\b`).MatchString(text)
	return f
}

// The selectors find on the recorded and reconstructed pages what the scripts take them for.
func TestGitHubSelectorsHoldOnPages(t *testing.T) {
	sel := loadGitHubPage(t, "select-target.html")
	rows := querySelectorAll(sel, ghTargetRowSel)
	var owners, hrefs []string
	for _, r := range rows {
		if o := querySelectorAll(r, ghRowOwnerSel); len(o) == 1 && strings.Contains(nodeText(r), "Configure") {
			owners = append(owners, strings.TrimSpace(nodeText(o[0])))
			hrefs = append(hrefs, attr(r, "href"))
		}
	}
	if !slices.Equal(owners, []string{"octo-user", "nwverse"}) ||
		!slices.Equal(hrefs, []string{"/settings/installations/10000001", "/apps/claude/installations/10000002"}) {
		t.Errorf("select_target rows: %v %v", owners, hrefs)
	}
	if f := factsOf(sel, "/apps/claude/installations/select_target"); f.Sudo || f.Captcha || !f.SignedIn || ghStop(f) != "" {
		t.Errorf("select_target is no stop: %+v", f)
	}

	sudo := factsOf(loadGitHubPage(t, "sudo.html"), "/settings/installations/10000001")
	if !sudo.Sudo || len(sudo.Targets) != 0 || len(sudo.IDs) != 0 {
		t.Errorf("confirm access: %+v", sudo)
	}
	if s, _ := ghSettingsState(sudo); s != provider.AuthorizeSudo {
		t.Errorf("confirm access state = %q", s)
	}

	sel2 := factsOf(loadGitHubPage(t, "settings-selected.html"), "/settings/installations/10000001")
	if sel2.Target != "selected" || !slices.Equal(sel2.IDs, []string{"111111111", "222222222"}) || !slices.Contains(sel2.Submits, "Save") ||
		!slices.Equal(sel2.Targets, []string{"all", "selected"}) {
		t.Errorf("settings (selected): %+v", sel2)
	}
	if s, all := ghSettingsState(sel2); s != "" || all {
		t.Errorf("settings (selected) state = %q %v, want go on", s, all)
	}

	all := factsOf(loadGitHubPage(t, "settings-all.html"), "/settings/installations/10000001")
	if s, isAll := ghSettingsState(all); s != provider.AuthorizeAlready || !isAll {
		t.Errorf("settings (all) state = %q %v", s, isAll)
	}

	req := factsOf(loadGitHubPage(t, "settings-request.html"), "/apps/claude/installations/10000002")
	if s, _ := ghSettingsState(req); s != provider.AuthorizeApproval {
		t.Errorf("settings (request) state = %q (%+v)", s, req)
	}

	login := factsOf(loadGitHubPage(t, "settings-selected.html"), "/login")
	if s, _ := ghSettingsState(login); s != provider.AuthorizeSignIn {
		t.Errorf("login state = %q", s)
	}
	if s, _ := ghSettingsState(factsOf(loadGitHubPage(t, "settings-selected.html"), "/orgs/nwverse/sso")); s != provider.AuthorizeSignIn {
		t.Errorf("SAML SSO state = %q", s)
	}
	requested := sel2
	requested.Request = true
	if s, _ := ghSettingsState(requested); s != provider.AuthorizeApproval {
		t.Errorf("a page speaking of a request is never saved: %q", s)
	}
	for _, must := range []string{`i.type === "hidden" || i.checked`, "orgs\\/[^/]+\\/sso", `meta[name="user-login"]`} {
		if !strings.Contains(ghProbeJS, must) {
			t.Errorf("probe lost %q", must)
		}
	}
	for _, must := range []string{"window.__pacSaving = true", "!window.__pacSaving"} {
		if !strings.Contains(ghSaveScript, must) {
			t.Errorf("save script does not wait for its response (%q)", must)
		}
	}
	captcha := sel2
	captcha.Captcha = true
	if s, _ := ghSettingsState(captcha); s != provider.AuthorizeHumanCheck {
		t.Errorf("captcha state = %q", s)
	}
}

// fakeGitHub answers ghRun's scripts: the probe with probe, the save with save, recording each save's arguments.
type fakeGitHub struct {
	probe ghProbeAnswer
	save  ghSaveAnswer
	saves []map[string]any
	slots []string
}

func (f *fakeGitHub) Name() string { return "fake" }
func (f *fakeGitHub) Machines(context.Context) ([]Machine, error) {
	return []Machine{{MachineID: "m1", Online: true, Ready: true}}, nil
}
func (f *fakeGitHub) Run(_ context.Context, req RunRequest) (RunResult, error) {
	f.slots = append(f.slots, req.Slot)
	line, _, _ := strings.Cut(strings.TrimPrefix(req.Script, "const ARGS = "), ";\n")
	var args map[string]any
	_ = json.Unmarshal([]byte(line), &args)
	var v any = f.probe
	if strings.Contains(req.Script, "requestSubmit") {
		f.saves = append(f.saves, args)
		v = f.save
	}
	b, _ := json.Marshal(map[string]any{"value": v})
	return RunResult{Backend: "ego", Output: string(b)}, nil
}

func probeOf(t *testing.T, page, path string, lookup map[string]string) ghProbeAnswer {
	t.Helper()
	f := factsOf(loadGitHubPage(t, page), path)
	return ghProbeAnswer{ConfigureURL: "https://github.com" + path, Facts: &f, Lookup: lookup}
}

func authorizeWith(t *testing.T, fg *fakeGitHub, repos ...string) provider.AuthorizeResult {
	t.Helper()
	a := New(SiteClaudeDuet, fg)
	res, err := a.AuthorizeRepos(context.Background(), `{"machine_id":"m1"}`, "octo-user", repos)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// Only the repositories asked for are added: the save is sent the ids the page had and only the new ones, and is
// done only when the page afterwards shows all of them with the grant still "selected".
func TestAuthorizeAddsOnlyTheAskedRepositories(t *testing.T) {
	path := "/settings/installations/10000001"
	fg := &fakeGitHub{probe: probeOf(t, "settings-selected.html", path, map[string]string{"openlinkedin": "900000001", "reachd": "222222222"})}
	after := *fg.probe.Facts
	after.IDs = []string{"111111111", "222222222", "900000001"}
	fg.save = ghSaveAnswer{Submitted: true, After: &after}
	res := authorizeWith(t, fg, "octo-user/openlinkedin", "https://github.com/octo-user/reachd")
	if res.State != provider.AuthorizeDone || !slices.Equal(res.Added, []string{"https://github.com/octo-user/openlinkedin"}) ||
		res.URL != "https://github.com"+path {
		t.Fatalf("result: %+v", res)
	}
	if len(fg.saves) != 1 {
		t.Fatalf("saves: %d", len(fg.saves))
	}
	s := fg.saves[0]
	if !slices.Equal(asStrings(s["before"]), []string{"111111111", "222222222"}) || !slices.Equal(asStrings(s["add"]), []string{"900000001"}) {
		t.Errorf("save args: %+v", s)
	}
	for _, slot := range fg.slots {
		if slot != ghSlot {
			t.Errorf("ran in slot %q, want its own %q (not the agent's)", slot, ghSlot)
		}
	}
	// The save script itself never touches the radio and refuses a merge that is not exact.
	for _, must := range []string{`f.target === "selected"`, "A.before.every", "now.length !== want.length", `/^save$/i`} {
		if !strings.Contains(ghSaveScript, must) {
			t.Errorf("save script lost its check %q", must)
		}
	}
	if strings.Contains(ghSaveScript, `value = "all"`) || strings.Contains(ghSaveScript, ".checked =") || strings.Contains(ghSaveScript, "remove()") {
		t.Error("save script changes the radio or removes an input")
	}
}

func asStrings(v any) []string {
	var out []string
	for _, x := range v.([]any) {
		out = append(out, x.(string))
	}
	return out
}

// Every stop leaves the page alone: no save is sent, and the result says where the owner goes.
func TestAuthorizeStopsWithoutSaving(t *testing.T) {
	path := "/settings/installations/10000001"
	sudoFacts := factsOf(loadGitHubPage(t, "sudo.html"), path)
	loginFacts := factsOf(loadGitHubPage(t, "select-target.html"), "/login")
	for _, tc := range []struct {
		name  string
		probe ghProbeAnswer
		want  provider.AuthorizeState
		url   string
	}{
		{"confirm access", ghProbeAnswer{ConfigureURL: "https://github.com" + path, Facts: &sudoFacts}, provider.AuthorizeSudo, "https://github.com" + path},
		{"signed out", ghProbeAnswer{Install: &loginFacts}, provider.AuthorizeSignIn, claudeGitHubApp.installURL()},
		{"not installed on the owner", ghProbeAnswer{None: true}, provider.AuthorizeNotInstalled, claudeGitHubApp.installURL()},
		{"an owner's approval", probeOf(t, "settings-request.html", "/apps/claude/installations/10000002", nil), provider.AuthorizeApproval, "https://github.com/apps/claude/installations/10000002"},
		{"all repositories", probeOf(t, "settings-all.html", path, nil), provider.AuthorizeAlready, "https://github.com" + path},
		{"no such repository", probeOf(t, "settings-selected.html", path, map[string]string{}), provider.AuthorizeNotFound, "https://github.com" + path},
		{"already selected", probeOf(t, "settings-selected.html", path, map[string]string{"openlinkedin": "111111111"}), provider.AuthorizeAlready, "https://github.com" + path},
	} {
		fg := &fakeGitHub{probe: tc.probe}
		res := authorizeWith(t, fg, "octo-user/openlinkedin")
		if res.State != tc.want || res.URL != tc.url || len(fg.saves) != 0 {
			t.Errorf("%s: %+v, %d saves", tc.name, res, len(fg.saves))
		}
		if res.State.Stopped() != (tc.want != provider.AuthorizeAlready) {
			t.Errorf("%s: Stopped = %v", tc.name, res.State.Stopped())
		}
	}
	// Each stop a person has to answer is named the same way when it shows up only at the save.
	fg := &fakeGitHub{probe: probeOf(t, "settings-selected.html", path, map[string]string{"openlinkedin": "900000001"}),
		save: ghSaveAnswer{Stop: &sudoFacts}}
	if res := authorizeWith(t, fg, "octo-user/openlinkedin"); res.State != provider.AuthorizeSudo {
		t.Errorf("confirm access at the save: %+v", res)
	}
}

// What the page shows after a save decides: a repository taken out or the grant switched to all is not done.
func TestAuthorizeChecksThePageAfterSaving(t *testing.T) {
	path := "/settings/installations/10000001"
	lookup := map[string]string{"openlinkedin": "900000001"}
	for _, tc := range []struct {
		name string
		save func(f ghFacts) ghSaveAnswer
		want provider.AuthorizeState
	}{
		{"one taken out", func(f ghFacts) ghSaveAnswer {
			f.IDs = []string{"111111111", "900000001"}
			return ghSaveAnswer{Submitted: true, After: &f}
		}, provider.AuthorizeUnconfirmed},
		{"switched to all", func(f ghFacts) ghSaveAnswer {
			f.Target, f.IDs = "all", []string{"111111111", "222222222", "900000001"}
			return ghSaveAnswer{Submitted: true, After: &f}
		}, provider.AuthorizeUnconfirmed},
		{"not read back", func(ghFacts) ghSaveAnswer { return ghSaveAnswer{Submitted: true} }, provider.AuthorizeUnconfirmed},
		{"page changed before the save", func(f ghFacts) ghSaveAnswer { return ghSaveAnswer{Changed: &f} }, provider.AuthorizeUnrecognized},
		{"merge not exact", func(ghFacts) ghSaveAnswer { return ghSaveAnswer{Merge: []string{"1"}} }, provider.AuthorizeUnrecognized},
		{"no Save button", func(f ghFacts) ghSaveAnswer { return ghSaveAnswer{NoSave: &f} }, provider.AuthorizeUnrecognized},
	} {
		fg := &fakeGitHub{probe: probeOf(t, "settings-selected.html", path, lookup)}
		fg.save = tc.save(*fg.probe.Facts)
		if res := authorizeWith(t, fg, "octo-user/openlinkedin"); res.State != tc.want || len(res.Added) != 0 {
			t.Errorf("%s: %+v", tc.name, res)
		}
	}
}

// Repositories of another owner than the installation's, and words that name none, are refused before any page.
func TestAuthorizeRefusesOtherOwnersAndBadNames(t *testing.T) {
	a := New(SiteClaudeDuet, &fakeGitHub{})
	for _, repos := range [][]string{{"someone-else/repo"}, {"not a repo"}} {
		if _, err := a.AuthorizeRepos(context.Background(), `{"machine_id":"m1"}`, "octo-user", repos); err == nil {
			t.Errorf("%v accepted", repos)
		}
	}
	if !New(SiteClaudeDuet, nil).MountsRepos() || New(SiteMuse, nil).MountsRepos() {
		t.Error("only Claude Duet mounts repositories")
	}
	if _, err := New(SiteMuse, &fakeGitHub{}).AuthorizeRepos(context.Background(), `{"machine_id":"m1"}`, "o", []string{"o/r"}); err == nil {
		t.Error("Muse authorized repositories")
	}
}
