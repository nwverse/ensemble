package runtime

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/nwverse/personal-agent-club/internal/core"
	"github.com/nwverse/personal-agent-club/internal/protocol"
	"github.com/nwverse/personal-agent-club/internal/provider"
)

// ClubClient abstracts in-process (*core.Club) and out-of-process (`serve` HTTP+SSE) access to club.
type ClubClient interface {
	Status(ctx context.Context) (protocol.StatusResult, error)
	Sessions(ctx context.Context) (protocol.SessionsResult, error)
	Messages(ctx context.Context, p protocol.MessagesParams) (protocol.MessagePage, error)
	Send(ctx context.Context, p protocol.SendParams) (protocol.Message, error)
	DeleteMessage(ctx context.Context, p protocol.DeleteMessageParams) error
	SyncAccount(ctx context.Context, accountID string) error
	Live(ctx context.Context, sessionKey string) (protocol.LiveState, error)
	StartSession(ctx context.Context, p protocol.StartSessionParams) (protocol.Session, error)
	InterruptSession(ctx context.Context, sessionKey string) error
	SetAssistantSessions(ctx context.Context, keys []string) error
	Subscribe(ctx context.Context) (<-chan protocol.UpdatedEvent, func())
	CallTools(ctx context.Context, accountID string, turns []provider.ToolTurn, tools []provider.ToolDef) (provider.ToolStepResult, bool, error)
}

// DirectClient talks to an in-process *core.Club.
type DirectClient struct {
	club  *core.Club
	space string
}

var _ ClubClient = (*DirectClient)(nil)

// NewDirectClient wraps club for the named space (default: core.DefaultSpace).
func NewDirectClient(club *core.Club, space string) *DirectClient {
	if strings.TrimSpace(space) == "" {
		space = core.DefaultSpace
	}
	return &DirectClient{club: club, space: space}
}

func (c *DirectClient) call(ctx context.Context, method string, params any) (any, error) {
	var raw json.RawMessage
	if params != nil {
		b, err := json.Marshal(params)
		if err != nil {
			return nil, err
		}
		raw = b
	}
	return c.club.Call(ctx, c.space, method, raw)
}

func (c *DirectClient) Status(ctx context.Context) (protocol.StatusResult, error) {
	res, err := c.call(ctx, "agentsrc.status", nil)
	if err != nil {
		return protocol.StatusResult{}, err
	}
	return res.(protocol.StatusResult), nil
}

func (c *DirectClient) Sessions(ctx context.Context) (protocol.SessionsResult, error) {
	res, err := c.call(ctx, "agentsrc.sessions", nil)
	if err != nil {
		return protocol.SessionsResult{}, err
	}
	return res.(protocol.SessionsResult), nil
}

func (c *DirectClient) Messages(ctx context.Context, p protocol.MessagesParams) (protocol.MessagePage, error) {
	res, err := c.call(ctx, "agentsrc.messages", p)
	if err != nil {
		return protocol.MessagePage{}, err
	}
	return res.(protocol.MessagePage), nil
}

func (c *DirectClient) Send(ctx context.Context, p protocol.SendParams) (protocol.Message, error) {
	res, err := c.call(ctx, "agentsrc.send", p)
	if err != nil {
		return protocol.Message{}, err
	}
	return res.(protocol.Message), nil
}

func (c *DirectClient) DeleteMessage(ctx context.Context, p protocol.DeleteMessageParams) error {
	_, err := c.call(ctx, "agentsrc.delete_message", p)
	return err
}

func (c *DirectClient) SyncAccount(ctx context.Context, accountID string) error {
	_, err := c.call(ctx, protocol.MethodClubAccountsSync, protocol.AccountParams{AccountID: accountID})
	return err
}

func (c *DirectClient) Live(ctx context.Context, sessionKey string) (protocol.LiveState, error) {
	res, err := c.call(ctx, protocol.MethodClubLive, protocol.SessionParams{SessionKey: sessionKey})
	if err != nil {
		return protocol.LiveState{}, err
	}
	return res.(protocol.LiveState), nil
}

func (c *DirectClient) StartSession(ctx context.Context, p protocol.StartSessionParams) (protocol.Session, error) {
	res, err := c.call(ctx, protocol.MethodClubSessionsStart, p)
	if err != nil {
		return protocol.Session{}, err
	}
	return res.(protocol.Session), nil
}

func (c *DirectClient) InterruptSession(ctx context.Context, sessionKey string) error {
	_, err := c.call(ctx, protocol.MethodClubSessionsInterrupt, protocol.SessionParams{SessionKey: sessionKey})
	return err
}

func (c *DirectClient) SetAssistantSessions(ctx context.Context, keys []string) error {
	_, err := c.call(ctx, protocol.MethodClubAssistantSessionsSet, protocol.AssistantSessionsParams{SessionKeys: keys})
	return err
}

func (c *DirectClient) Subscribe(ctx context.Context) (<-chan protocol.UpdatedEvent, func()) {
	sub := c.club.Subscribe(c.space)
	ch := make(chan protocol.UpdatedEvent, 64)
	sctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer sub.Close()
		defer close(ch)
		for {
			select {
			case <-sctx.Done():
				return
			case <-sub.Ready():
				for _, ev := range sub.Take() {
					select {
					case ch <- protocol.UpdatedEvent{SessionKeys: ev.SessionKeys}:
					default:
					}
				}
			}
		}
	}()
	return ch, func() {
		cancel()
		sub.Close()
		<-done
	}
}

func (c *DirectClient) SupportsTools(ctx context.Context, accountID string) bool {
	sp, err := c.club.Space(ctx, c.space)
	if err != nil {
		return false
	}
	return sp.SupportsTools(ctx, accountID)
}

func (c *DirectClient) CallTools(ctx context.Context, accountID string, turns []provider.ToolTurn, tools []provider.ToolDef) (provider.ToolStepResult, bool, error) {
	sp, err := c.club.Space(ctx, c.space)
	if err != nil {
		return provider.ToolStepResult{}, false, err
	}
	return sp.CallTools(ctx, accountID, turns, tools)
}

// ServeClient talks to a running `personal-agent-club serve` instance over unix socket or HTTP URL.
type ServeClient struct {
	httpClient *http.Client
	sseClient  *http.Client
	baseURL    string
	space      string
	token      string
}

var _ ClubClient = (*ServeClient)(nil)

// NewServeClient creates a ClubClient talking to a `personal-agent-club serve` unix socket path or http(s) URL.
func NewServeClient(target, space, token string) (*ServeClient, error) {
	target = strings.TrimSpace(target)
	if target == "" {
		return nil, errors.New("serve target (socket or URL) is required")
	}
	if strings.TrimSpace(space) == "" {
		space = core.DefaultSpace
	}
	if strings.HasPrefix(target, "http://") || strings.HasPrefix(target, "https://") {
		if _, err := url.Parse(target); err != nil {
			return nil, err
		}
		return &ServeClient{
			httpClient: &http.Client{Timeout: 2 * time.Minute},
			sseClient:  &http.Client{},
			baseURL:    strings.TrimRight(target, "/"),
			space:      space,
			token:      token,
		}, nil
	}
	dialer := func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", target)
	}
	return &ServeClient{
		httpClient: &http.Client{Timeout: 2 * time.Minute, Transport: &http.Transport{DialContext: dialer}},
		sseClient:  &http.Client{Transport: &http.Transport{DialContext: dialer}},
		baseURL:    "http://club",
		space:      space,
		token:      token,
	}, nil
}

func (c *ServeClient) rpc(ctx context.Context, method string, params, out any) error {
	body, err := json.Marshal(map[string]any{"method": method, "params": params})
	if err != nil {
		return err
	}
	endpoint := fmt.Sprintf("%s/v1/spaces/%s/rpc", c.baseURL, url.PathEscape(c.space))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return err
	}
	var envelope struct {
		Result json.RawMessage `json:"result"`
		Error  *protocol.Error `json:"error"`
	}
	if uErr := json.Unmarshal(raw, &envelope); uErr != nil {
		return fmt.Errorf("HTTP %d: %w", resp.StatusCode, uErr)
	}
	if envelope.Error != nil {
		return envelope.Error
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	if out != nil && len(envelope.Result) > 0 && string(envelope.Result) != "null" {
		return json.Unmarshal(envelope.Result, out)
	}
	return nil
}

func (c *ServeClient) Status(ctx context.Context) (protocol.StatusResult, error) {
	var res protocol.StatusResult
	return res, c.rpc(ctx, "agentsrc.status", nil, &res)
}

func (c *ServeClient) Sessions(ctx context.Context) (protocol.SessionsResult, error) {
	var res protocol.SessionsResult
	return res, c.rpc(ctx, "agentsrc.sessions", nil, &res)
}

func (c *ServeClient) Messages(ctx context.Context, p protocol.MessagesParams) (protocol.MessagePage, error) {
	var res protocol.MessagePage
	return res, c.rpc(ctx, "agentsrc.messages", p, &res)
}

func (c *ServeClient) Send(ctx context.Context, p protocol.SendParams) (protocol.Message, error) {
	var res protocol.Message
	return res, c.rpc(ctx, "agentsrc.send", p, &res)
}

func (c *ServeClient) DeleteMessage(ctx context.Context, p protocol.DeleteMessageParams) error {
	return c.rpc(ctx, "agentsrc.delete_message", p, nil)
}

func (c *ServeClient) SyncAccount(ctx context.Context, accountID string) error {
	return c.rpc(ctx, protocol.MethodClubAccountsSync, protocol.AccountParams{AccountID: accountID}, nil)
}

func (c *ServeClient) Live(ctx context.Context, sessionKey string) (protocol.LiveState, error) {
	var res protocol.LiveState
	return res, c.rpc(ctx, protocol.MethodClubLive, protocol.SessionParams{SessionKey: sessionKey}, &res)
}

func (c *ServeClient) StartSession(ctx context.Context, p protocol.StartSessionParams) (protocol.Session, error) {
	var res protocol.Session
	return res, c.rpc(ctx, protocol.MethodClubSessionsStart, p, &res)
}

func (c *ServeClient) InterruptSession(ctx context.Context, sessionKey string) error {
	return c.rpc(ctx, protocol.MethodClubSessionsInterrupt, protocol.SessionParams{SessionKey: sessionKey}, nil)
}

func (c *ServeClient) SetAssistantSessions(ctx context.Context, keys []string) error {
	return c.rpc(ctx, protocol.MethodClubAssistantSessionsSet, protocol.AssistantSessionsParams{SessionKeys: keys}, nil)
}

func (c *ServeClient) Subscribe(ctx context.Context) (<-chan protocol.UpdatedEvent, func()) {
	ch := make(chan protocol.UpdatedEvent, 64)
	sctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer close(ch)
		endpoint := fmt.Sprintf("%s/v1/spaces/%s/events", c.baseURL, url.PathEscape(c.space))
		for sctx.Err() == nil {
			req, err := http.NewRequestWithContext(sctx, http.MethodGet, endpoint, nil)
			if err != nil {
				return
			}
			if c.token != "" {
				req.Header.Set("Authorization", "Bearer "+c.token)
			}
			resp, err := c.sseClient.Do(req)
			if err == nil && resp.StatusCode == http.StatusOK {
				sc := bufio.NewScanner(resp.Body)
				sc.Buffer(make([]byte, 16*1024), 4<<20)
				var evName string
				for sc.Scan() {
					line := sc.Text()
					switch {
					case line == "":
						evName = ""
					case strings.HasPrefix(line, "event: "):
						evName = strings.TrimPrefix(line, "event: ")
					case strings.HasPrefix(line, "data: "):
						if evName == protocol.EventUpdated {
							var ev protocol.UpdatedEvent
							if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev) == nil {
								select {
								case ch <- ev:
								default:
								}
							}
						}
					}
				}
				_ = resp.Body.Close()
			} else if resp != nil {
				_ = resp.Body.Close()
			}
			select {
			case <-sctx.Done():
				return
			case <-time.After(time.Second):
			}
		}
	}()
	return ch, func() {
		cancel()
		<-done
	}
}

func (c *ServeClient) CallTools(context.Context, string, []provider.ToolTurn, []provider.ToolDef) (provider.ToolStepResult, bool, error) {
	return provider.ToolStepResult{}, false, nil
}
