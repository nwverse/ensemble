package main

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRuntimeFlags(t *testing.T) {
	u := newUpstream(t)
	data := filepath.Join(t.TempDir(), "data")
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"runtime", "-data", data, "-space", "../bad"}, "invalid space"},
		{[]string{"runtime", "-data", data, "-demo-echo"}, "-standalone"},
		{[]string{"runtime", "-data", data, "extra"}, "unexpected argument"},
	} {
		d, _, errOut := testDeps(u)
		if code := runWith(tc.args, d); code != 2 || !strings.Contains(errOut.String(), tc.want) {
			t.Fatalf("%v: code=%d stderr=%q", tc.args, code, errOut.String())
		}
	}
	if _, err := os.Stat(data); err == nil {
		t.Fatal("refused runtime flags created data directory")
	}
}

func TestRuntimeStandaloneHandshake(t *testing.T) {
	u := newUpstream(t)
	data := shortDir(t)
	sockDir := shortDir(t)
	personaSock := filepath.Join(sockDir, "runtime.sock")

	ln, err := net.Listen("unix", personaSock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	helloSeen := make(chan string, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		sc := bufio.NewScanner(conn)
		for sc.Scan() {
			var f struct {
				ID     string         `json:"id"`
				Method string         `json:"method"`
				Params map[string]any `json:"params"`
			}
			if json.Unmarshal(sc.Bytes(), &f) != nil {
				continue
			}
			if f.Method == "runtime.hello" {
				raw, _ := json.Marshal(f.Params)
				select {
				case helloSeen <- string(raw):
				default:
				}
				resp, _ := json.Marshal(map[string]any{
					"id": f.ID,
					"result": map[string]any{
						"protocol":     1,
						"node_version": "test",
						"hub":          true,
						"features":     []string{"launchers"},
					},
				})
				_, _ = conn.Write(append(resp, '\n'))
				return
			}
		}
	}()

	d, _, errOut := testDeps(u)
	ctx, cancel := context.WithCancel(context.Background())
	d.ctx = ctx
	defer cancel()

	codeCh := make(chan int, 1)
	go func() {
		codeCh <- runWith([]string{
			"runtime",
			"-data", data,
			"-standalone",
			"-demo-echo",
			"-all-accounts",
			"-persona-socket", personaSock,
		}, d)
	}()

	select {
	case rawHello := <-helloSeen:
		if !strings.Contains(rawHello, `"assistant":true`) || !strings.Contains(rawHello, `"personal-agent-club"`) {
			t.Fatalf("unexpected runtime.hello: %s", rawHello)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for runtime.hello (stderr=%s)", errOut.String())
	}

	cancel()
	if code := <-codeCh; code != 0 {
		t.Fatalf("runtime exited %d: %s", code, errOut.String())
	}
	noSecrets(t, errOut.String())
}
