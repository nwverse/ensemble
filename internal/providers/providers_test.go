package providers

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/nwverse/ensemble/internal/protocol"
)

func TestRegistryHasTheAPIProviders(t *testing.T) {
	r, err := Registry(Config{Proxy: "socks5h://127.0.0.1:1080"})
	if err != nil {
		t.Fatal(err)
	}
	available := map[string]bool{}
	for _, p := range r.List() {
		available[p.Provider] = p.Available
		if p.Available && p.Connection != protocol.ConnectionServer {
			t.Fatalf("%+v", p)
		}
	}
	want := map[string]bool{"cue": true, "grok_bot": true, "groq": true, "muse2api": false, "muse": false, "dots": false,
		"claude_duet": false}
	for name, ok := range want {
		if available[name] != ok {
			t.Errorf("%s available %v", name, available[name])
		}
	}
	if _, err := Registry(Config{Proxy: "nope"}); err == nil {
		t.Fatal("a bad proxy was accepted")
	}
}

// muse2api is in only when its server's address is configured, and a bad address is refused.
func TestRegistryAddsMuse2APIWithItsAddress(t *testing.T) {
	r, err := Registry(Config{Muse2APIURL: "http://192.0.2.7:18610", Muse2APIModel: "gpt-4o"})
	if err != nil {
		t.Fatal(err)
	}
	a, ok := r.Get("muse2api")
	if !ok || a.Connection() != protocol.ConnectionServer {
		t.Fatalf("muse2api: %v", ok)
	}
	for _, p := range r.List() {
		if p.Provider == "muse2api" && !p.Available {
			t.Fatalf("%+v", p)
		}
	}
	if _, err := Registry(Config{Muse2APIURL: "muse.example.com"}); err == nil {
		t.Fatal("a muse2api address without a scheme was accepted")
	}
}

func TestFromEnv(t *testing.T) {
	t.Setenv(EnvProxy, " socks5://127.0.0.1:1080 ")
	t.Setenv(EnvGroqModel, "llama-3.3-70b-versatile")
	t.Setenv(EnvMuse2APIURL, " http://192.0.2.7:18610 ")
	t.Setenv(EnvMuse2APIModel, "muse-spark")
	if c := FromEnv(); c.Proxy != "socks5://127.0.0.1:1080" || c.GroqModel != "llama-3.3-70b-versatile" ||
		c.Muse2APIURL != "http://192.0.2.7:18610" || c.Muse2APIModel != "muse-spark" {
		t.Fatalf("%+v", c)
	}
}

// A host's own client carries every provider request (here, through a fake "proxy" transport) and still never
// follows a redirect.
func TestRegistryUsesAHandedClient(t *testing.T) {
	var hosts []string
	rt := roundTrip(func(r *http.Request) (*http.Response, error) {
		hosts = append(hosts, r.URL.Host)
		h := http.Header{"Location": {"https://elsewhere.example/"}, "Content-Type": {"application/json"}}
		return &http.Response{StatusCode: http.StatusFound, Header: h, Body: io.NopCloser(strings.NewReader("{}")), Request: r}, nil
	})
	r, err := Registry(Config{Client: &http.Client{Transport: rt}, DemoEcho: true})
	if err != nil {
		t.Fatal(err)
	}
	groq, _ := r.Get("groq")
	if _, err := groq.Verify(context.Background(), "fixture-key-0001"); err == nil {
		t.Fatal("a redirect was followed or accepted")
	}
	if len(hosts) != 1 || hosts[0] != "api.groq.com" {
		t.Fatalf("requests went to %v", hosts)
	}
	if _, ok := r.Get("echo"); !ok {
		t.Fatal("DemoEcho did not add echo")
	}
	if _, err := Registry(Config{Client: http.DefaultClient, Proxy: "socks5://127.0.0.1:1080"}); err == nil {
		t.Fatal("a client and a proxy at once were accepted")
	}
}

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
