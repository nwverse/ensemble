package browser

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"
)

// A space's tab is not reloaded for every read, and it is closed now and then (#46).
//
// Reloading a site in the same tab keeps it in the same renderer process, and what each load leaves behind adds up
// there: ChatGPT's renderer grew by 1–2 GB an hour when every poll reloaded chatgpt.com. So a read that only calls
// the site's API from inside the page stays on the page it finds, when that page is the site's and has loaded
// (openJS). Only closing the tab lets the renderer go, so after TabRecycle's runs or age a read closes the space's
// tab once it has its answer, and the next run opens a fresh one.

// TabRecycle is when a space's tab is closed and reopened: after Runs reads in it, or once it has been in use for
// Age, whichever comes first. Zero turns that limit off.
type TabRecycle struct {
	Runs int
	Age  time.Duration
}

// DefaultTabRecycle closes a space's tab every 120 reads or 30 minutes. A space polled every minute, with a few
// reads each time, has its tab closed about twice an hour instead of reloading it about 90 times.
var DefaultTabRecycle = TabRecycle{Runs: 120, Age: 30 * time.Minute}

// openJS opens gotoURL in page unless page is already on a loaded document whose address matches stay (a JavaScript
// regular expression), and always when stay is empty (a send needs the composer of one page). A run whose
// previous answer in this tab failed sets __pacReload, so a page left signed out or behind a human check is loaded
// again instead of being kept.
func openJS(gotoURL, stay string) string {
	u, _ := json.Marshal(gotoURL)
	if stay == "" {
		return fmt.Sprintf(`
await page.goto(%s);
await page.waitForLoadState("domcontentloaded").catch(() => {});
`, u)
	}
	s, _ := json.Marshal(stay)
	// A page that cannot be looked into or is still loading may be another run's navigation in this tab (a Gemini
	// send opening its task): it is waited for, and then its address alone decides, as before, rather than being
	// navigated away from.
	return fmt.Sprintf(`
const __stay = new RegExp(%s);
const __look = () => page.evaluate(() => ({ href: String(location.href), state: String(document.readyState) })).catch(() => null);
let __here = false;
if (!(typeof __pacReload !== "undefined" && __pacReload)) {
  let at = await __look();
  if (!at || at.state === "loading") {
    await page.waitForLoadState("domcontentloaded").catch(() => {});
    at = await __look();
  }
  __here = at ? at.state !== "loading" && __stay.test(at.href) :
    __stay.test(String(await Promise.resolve().then(() => page.url()).catch(() => "")));
}
if (!__here) {
  await page.goto(%s);
  await page.waitForLoadState("domcontentloaded").catch(() => {});
}
`, s, u)
}

// reloadScript makes script load its page whatever it finds there (openJS).
func reloadScript(script string) string {
	return "const __pacReload = true;\n" + script
}

// recycleScript runs script and then, when it answered without an error, closes the task space's tabs (the next
// run in the space opens a fresh one), saying so in the answer's tabRecycled. An answer with an error leaves the
// tab as it is: a person may have to sign in or pass a human check in it. Nor is a tab closed whose answer says the
// page is busy: the site is still writing an answer there that it loses when the page goes (Gemini's).
func recycleScript(script string) string {
	return `
const __v = await (async () => {
` + script + `
})();
if (__v && typeof __v === "object" && !__v.error && !__v.busy && typeof task !== "undefined" && task && typeof task.finish === "function") {
  try { await task.finish({ keep: [] }); __v.tabRecycled = true; } catch (e) {}
}
return __v;
`
}

// tabs counts the runs in each space's tab (one per computer and backend: an Adapter has one slot).
type tabs struct {
	policy TabRecycle
	now    func() time.Time

	mu  sync.Mutex
	use map[string]*tabUse
}

type tabUse struct {
	runs     int           // reads since the tab was opened
	since    time.Time     // when the first of them ran
	reload   bool          // the next read loads its page again (the last answer in the tab failed)
	inflight int           // runs in the tab now
	closing  chan struct{} // closed when the run that closes the tab ends; nil when none does
}

func newTabs(policy TabRecycle) *tabs {
	return &tabs{policy: policy, now: time.Now, use: map[string]*tabUse{}}
}

// tabLease is one run in a tab: whether it loads its page again and whether it closes the tab at the end.
type tabLease struct {
	key     string
	read    bool
	reload  bool
	recycle bool
}

// begin starts a run in key's tab, after a run closing it has ended. A read is told to load its page again when the
// last answer in the tab failed (and nothing else runs in the tab), and to close the tab when the tab is due and
// nothing else runs in it. A send is never told to close the tab, and nothing closes it under a running send.
func (t *tabs) begin(ctx context.Context, key string, read bool) (tabLease, error) {
	for {
		t.mu.Lock()
		u := t.use[key]
		if u == nil {
			u = &tabUse{since: t.now()}
			t.use[key] = u
		}
		if ch := u.closing; ch != nil {
			t.mu.Unlock()
			select {
			case <-ch:
				continue
			case <-ctx.Done():
				return tabLease{}, ctx.Err()
			}
		}
		u.inflight++
		l := tabLease{key: key, read: read}
		if read {
			u.runs++
			// Not under another run: a send waiting in this tab (Gemini's) is not navigated away from.
			l.reload = u.reload && u.inflight == 1
			if u.inflight == 1 && t.due(u) {
				l.recycle = true
				u.closing = make(chan struct{})
			}
		}
		t.mu.Unlock()
		return l, nil
	}
}

func (t *tabs) due(u *tabUse) bool {
	return (t.policy.Runs > 0 && u.runs >= t.policy.Runs) || (t.policy.Age > 0 && t.now().Sub(u.since) >= t.policy.Age)
}

// end finishes l: failed when the run or its answer failed, recycled when its answer says the tab was closed.
func (t *tabs) end(l tabLease, failed, recycled bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	u := t.use[l.key]
	if u == nil {
		return
	}
	u.inflight--
	if l.recycle {
		close(u.closing)
		u.closing = nil
	}
	if recycled {
		// The next run opens a blank tab, which openJS loads anyway.
		u.runs, u.since, u.reload = 0, t.now(), false
		return
	}
	u.reload = failed
}
