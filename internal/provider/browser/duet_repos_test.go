package browser

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

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
	// The page's own merge keeps what the channel has then, and sends only default_source_urls.
	for _, must := range []string{"const before = await read()", "next = before.slice()", `method: "PATCH"`,
		"JSON.stringify({ default_source_urls: next })", "if (next.length === before.length) return"} {
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
