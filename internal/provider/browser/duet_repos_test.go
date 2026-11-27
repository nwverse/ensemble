package browser

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nwverse/ensemble/internal/provider"
)

// testdata/duet: claude.ai's answers in the shapes read on 2026-11-27 (code-repos.json: GET
// /api/organizations/<org>/code/repos; channel-config-*.json: GET /v1/code/channels/<chan>/config), the account's
// repositories replaced with public ones.

func readDuetJSON(t *testing.T, name string, v any) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "duet", name))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatal(err)
	}
}

// The fields the repos script reads are where claude.ai puts them.
func TestDuetInventoryFromRecordedShapes(t *testing.T) {
	var repos struct {
		Repos []struct {
			Repo struct {
				Private     bool `json:"private"`
				Permissions struct {
					Push bool `json:"push"`
				} `json:"permissions"`
			} `json:"repo"`
			SourceURL string `json:"source_url"`
		} `json:"repos"`
		IsComplete bool `json:"is_complete"`
	}
	readDuetJSON(t, "code-repos.json", &repos)
	var mainCfg, emptyCfg struct {
		Config struct {
			DefaultSourceURLs []string `json:"default_source_urls"`
		} `json:"config"`
	}
	readDuetJSON(t, "channel-config-main.json", &mainCfg)
	readDuetJSON(t, "channel-config-empty.json", &emptyCfg)
	for _, must := range []string{"/code/repos", "source_url", "is_complete", "permissions.push", "/config", "default_source_urls"} {
		if !strings.Contains(duetReposScript, must) {
			t.Errorf("repos script does not read %q", must)
		}
	}

	var ans duetReposAnswer
	ans.Complete = repos.IsComplete
	for _, r := range repos.Repos {
		ans.Authorized = append(ans.Authorized, duetRepoAnswer{r.SourceURL, r.Repo.Private, r.Repo.Permissions.Push})
	}
	mainRepos, emptyRepos := mainCfg.Config.DefaultSourceURLs, emptyCfg.Config.DefaultSourceURLs
	ans.Channels = append(ans.Channels,
		duetChanAnswer{"chan_side1", "openlinkedin", false, &emptyRepos},
		duetChanAnswer{"chan_main1", "", true, &mainRepos},
		duetChanAnswer{"chan_unread", "unanswered", false, nil})
	inv := duetInventory(ans)
	if !inv.Complete || len(inv.Authorized) != 3 || !inv.IsAuthorized("NWVerse/Ensemble") || inv.IsAuthorized("nwverse/openlinkedin") ||
		!inv.Authorized[2].Private || !inv.Authorized[1].CanPush {
		t.Errorf("authorized: %+v", inv.Authorized)
	}
	if len(inv.Workspaces) != 2 || !inv.Workspaces[0].Main || inv.Workspaces[0].Name != "主聊天" || inv.Workspaces[0].URL != "https://claude.ai/duet" ||
		len(inv.Workspaces[0].Repos) != 2 || inv.Workspaces[1].ID != "chan_side1" || len(inv.Workspaces[1].Repos) != 0 {
		t.Errorf("workspaces (main first; a channel whose config did not answer left out): %+v", inv.Workspaces)
	}
	if inv.GrantURL != "https://github.com/apps/claude/installations/new" {
		t.Errorf("grant URL %q", inv.GrantURL)
	}
}

func TestMergeReposOnlyAdds(t *testing.T) {
	before := []string{"https://github.com/nwverse/persona-extensions", "https://github.com/nwverse/persona"}
	got := mergeRepos(before, []string{"https://github.com/NWVERSE/persona", "https://github.com/nwverse/openlinkedin"})
	if !slices.Equal(got, append(slices.Clone(before), "https://github.com/nwverse/openlinkedin")) {
		t.Errorf("merge = %v", got)
	}
	// The page's own merge keeps what the channel has then, and sends default_source_urls with the added ones chosen.
	for _, must := range []string{"const before = urls(await __api(path))", "next = before.slice()", `method: "PATCH"`,
		"JSON.stringify({ default_source_urls: next, chosen_source_urls: added })", "if (added.length === 0) return"} {
		if !strings.Contains(duetMountScript, must) {
			t.Errorf("mount script lost %q", must)
		}
	}
	if !strings.Contains(duetMountScript, `!Array.isArray(cfg.config.default_source_urls)) throw new Error("shape")`) {
		t.Error("mount script writes back a config it could not read")
	}
	if strings.Contains(duetMountScript, "splice") || strings.Contains(duetMountScript, "filter(") {
		t.Error("mount script may take a repository out")
	}
}

// fakeDuetDriver answers the mount script with answer, keeping the script.
type fakeDuetDriver struct {
	answer  any
	scripts []string
}

func (f *fakeDuetDriver) Name() string                                { return "fake" }
func (f *fakeDuetDriver) Machines(context.Context) ([]Machine, error) { return nil, nil }
func (f *fakeDuetDriver) Run(_ context.Context, req RunRequest) (RunResult, error) {
	f.scripts = append(f.scripts, req.Script)
	b, _ := json.Marshal(map[string]any{"value": f.answer})
	return RunResult{Backend: "ego", Output: string(b)}, nil
}

func TestDuetMountRepos(t *testing.T) {
	ctx := context.Background()
	drv := &fakeDuetDriver{answer: map[string]any{"before": []string{"https://github.com/nwverse/persona"},
		"after": []string{"https://github.com/nwverse/persona", "https://github.com/nwverse/openlinkedin"}}}
	a := New(SiteClaudeDuet, drv)
	res, err := a.MountRepos(ctx, `{"machine_id":"m1"}`, "chan_01CFwJZJtCoUJweZ64fQSYzu", []string{"nwverse/openlinkedin"})
	if err != nil || !res.Verified([]string{"https://github.com/nwverse/openlinkedin"}) || res.Refused != "" {
		t.Fatalf("mount: %+v %v", res, err)
	}
	if s := drv.scripts[0]; !strings.Contains(s, `"chan":"chan_01CFwJZJtCoUJweZ64fQSYzu"`) || !strings.Contains(s, `"add":["https://github.com/nwverse/openlinkedin"]`) {
		t.Errorf("script args: %s", s[:200])
	}

	drv.answer = map[string]any{"before": []string{}, "after": []string{}, "refused": "github_repo_access_denied"}
	if res, err := a.MountRepos(ctx, `{"machine_id":"m1"}`, "chan_x1", []string{"nwverse/openbilibili"}); err != nil || res.Refused != "github_repo_access_denied" {
		t.Errorf("refused: %+v %v", res, err)
	}
	drv.answer = map[string]any{"error": "auth"}
	if _, err := a.MountRepos(ctx, `{"machine_id":"m1"}`, "chan_x1", []string{"nwverse/openbilibili"}); provider.CodeOf(err) != provider.CodeAuthFailed {
		t.Errorf("signed out: %v", err)
	}
	if _, err := a.MountRepos(ctx, `{"machine_id":"m1"}`, "not-a-channel", []string{"o/r"}); err == nil {
		t.Error("a workspace that is not a channel was written")
	}
	if n := len(drv.scripts); n != 3 {
		t.Errorf("%d scripts ran, want 3 (none for the bad channel)", n)
	}
}

// The mount is written where Claude keeps a channel's repositories: its config (updateChannelConfig), read and
// written at one path. A PATCH of the channel itself (updateChannel) answers 200 and drops default_source_urls: the
// cause of every bind ending unconfirmed with mounted:[] (2026-11-27).
func TestDuetMountWritesTheChannelConfig(t *testing.T) {
	if !strings.Contains(duetMountScript, `const path = "/v1/code/channels/" + encodeURIComponent(ARGS.chan) + "/config";`) {
		t.Fatal("mount script does not address the channel's config")
	}
	if n := strings.Count(duetMountScript, "fetch("); n != 1 || !strings.Contains(duetMountScript, "await fetch(path, {") {
		t.Errorf("mount script writes somewhere else than the config (%d fetches)", n)
	}
	var ch struct {
		Channel map[string]json.RawMessage `json:"channel"`
	}
	readDuetJSON(t, "channel-patch-ignored.json", &ch)
	if _, ok := ch.Channel["default_source_urls"]; ok || ch.Channel["id"] == nil {
		t.Errorf("the channel resource has no default_source_urls: %v", ch.Channel)
	}

	// What a saved config answers: the repositories as saved (the read back when the GET after fails).
	var ok struct {
		Config struct {
			DefaultSourceURLs []string `json:"default_source_urls"`
		} `json:"config"`
	}
	readDuetJSON(t, "config-patch-ok.json", &ok)
	if len(ok.Config.DefaultSourceURLs) != 2 || !strings.Contains(duetMountScript, "after = urls(j)") {
		t.Errorf("saved config: %+v", ok)
	}

	// A refusal names its reason in error.resource (no error.reason): it is a refusal, not a signed-out page.
	var refused struct {
		Error struct {
			Type      string `json:"type"`
			Reason    string `json:"reason"`
			Resource  string `json:"resource"`
			SubReason string `json:"sub_reason"`
		} `json:"error"`
	}
	readDuetJSON(t, "config-patch-refused.json", &refused)
	if refused.Error.Reason != "" || refused.Error.Resource != "github_repo_access_denied" || refused.Error.Type != "permission_error" {
		t.Errorf("refusal shape: %+v", refused)
	}
	for _, must := range []string{"er.reason || er.resource || er.sub_reason", `r.status === 403 && !reason`} {
		if !strings.Contains(duetMountScript, must) {
			t.Errorf("mount script lost %q", must)
		}
	}
}

// blockingDuetDriver holds each run until released, counting how many run at once.
type blockingDuetDriver struct {
	mu      sync.Mutex
	now     int
	most    int
	started chan struct{}
	release chan struct{}
}

func (b *blockingDuetDriver) Name() string                                { return "fake" }
func (b *blockingDuetDriver) Machines(context.Context) ([]Machine, error) { return nil, nil }
func (b *blockingDuetDriver) Run(ctx context.Context, _ RunRequest) (RunResult, error) {
	b.mu.Lock()
	b.now++
	b.most = max(b.most, b.now)
	b.mu.Unlock()
	b.started <- struct{}{}
	select {
	case <-b.release:
	case <-ctx.Done():
	}
	b.mu.Lock()
	b.now--
	b.mu.Unlock()
	out, _ := json.Marshal(map[string]any{"value": map[string]any{"before": []string{}, "after": []string{"https://github.com/o/r"}}})
	return RunResult{Backend: "ego", Output: string(out)}, nil
}

// Two mounts on one channel never run together, even from two computers' browsers (two tabs, two adapters); a
// mount on another channel is not held up by them.
func TestDuetMountOneAtATimePerChannel(t *testing.T) {
	drv := &blockingDuetDriver{started: make(chan struct{}, 8), release: make(chan struct{})}
	a1, a2 := New(SiteClaudeDuet, drv), New(SiteClaudeDuet, drv)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	errs := make(chan error, 3)
	mount := func(a *Adapter, cred, ch string) {
		defer wg.Done()
		_, err := a.MountRepos(ctx, cred, ch, []string{"o/r"})
		errs <- err
	}
	wg.Add(2)
	go mount(a1, `{"machine_id":"m1"}`, "chan_same")
	go mount(a2, `{"machine_id":"m2"}`, "chan_same")
	<-drv.started
	select {
	case <-drv.started:
		t.Fatal("a second mount on the channel ran while the first had not ended")
	case <-time.After(100 * time.Millisecond):
	}
	wg.Add(1)
	go mount(a2, `{"machine_id":"m3"}`, "chan_other")
	select {
	case <-drv.started:
	case <-time.After(5 * time.Second):
		t.Fatal("a mount on another channel waited for this one")
	}
	close(drv.release)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Error(err)
		}
	}
	if drv.most != 2 {
		t.Errorf("%d ran at once, want 2 (one per channel)", drv.most)
	}

	// A wait for the channel ends with its context.
	unlock, err := duetChanLocks.lock(context.Background(), "chan_held")
	if err != nil {
		t.Fatal(err)
	}
	short, stop := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer stop()
	if _, err := a1.MountRepos(short, `{"machine_id":"m1"}`, "chan_held", []string{"o/r"}); provider.CodeOf(err) != provider.CodeNetworkError {
		t.Errorf("waiting past the context: %v", err)
	}
	unlock()
}
