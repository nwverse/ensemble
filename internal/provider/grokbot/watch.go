package grokbot

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"unicode/utf8"

	"github.com/nwverse/ensemble/internal/provider"
	"github.com/nwverse/ensemble/internal/provider/web"
)

// ConnectFrame is one Connect envelope: a flags byte, a big-endian length, then the JSON.
func ConnectFrame(message any) []byte {
	payload, _ := json.Marshal(message)
	out := make([]byte, 5, 5+len(payload))
	binary.BigEndian.PutUint32(out[1:], uint32(len(payload)))
	return append(out, payload...)
}

// ReadConnectFrames calls each with the messages of a Connect server stream until it ends, each returns false or
// ctx ends. Flag 2 is the end-of-stream message, which carries an error when the call failed; compressed or
// oversized frames are refused.
func ReadConnectFrames(ctx context.Context, r io.Reader, each func(map[string]any) bool) error {
	br := bufio.NewReader(r)
	var header [5]byte
	for {
		if ctx.Err() != nil {
			return nil
		}
		if _, err := io.ReadFull(br, header[:]); err != nil {
			if errors.Is(err, io.EOF) || ctx.Err() != nil {
				return nil
			}
			if errors.Is(err, io.ErrUnexpectedEOF) {
				return nil
			}
			return &provider.Error{Code: provider.CodeNetworkError, Retryable: true}
		}
		flags, length := header[0], binary.BigEndian.Uint32(header[1:])
		if length > maxFrame || flags&1 != 0 || (flags != 0 && flags != 2) {
			return &provider.Error{Code: provider.CodeRequestFailed}
		}
		payload := make([]byte, length)
		if _, err := io.ReadFull(br, payload); err != nil {
			if ctx.Err() != nil || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
				return nil
			}
			return &provider.Error{Code: provider.CodeNetworkError, Retryable: true}
		}
		var v any
		if !utf8.Valid(payload) || json.Unmarshal(payload, &v) != nil {
			return &provider.Error{Code: provider.CodeRequestFailed}
		}
		message := web.Record(v)
		if flags == 2 {
			if _, failed := message["error"]; failed {
				return &provider.Error{Code: provider.CodeUpstreamError, Retryable: true}
			}
			return nil
		}
		if !each(message) {
			return nil
		}
	}
}

// WatchEvent is what one watch frame means for a session: new or removed entries, a cleared transcript or a failed
// turn are a change to read; the session's live state with nothing running means the turn is over.
func WatchEvent(frame map[string]any, bot, session string) provider.WatchEvent {
	here := func(v any) bool {
		row := web.Record(v)
		return web.Text(row["agentId"]) == bot && web.Text(row["sessionId"]) == session
	}
	rows := web.Record(frame["rows"])
	if here(rows) && (len(web.Records(rows["entries"])) > 0 || len(web.Records(rows["deletes"])) > 0) {
		return provider.WatchChanged
	}
	if here(frame["cleared"]) || here(frame["turnFailed"]) {
		return provider.WatchChanged
	}
	var live []map[string]any
	for _, row := range web.Records(web.Record(frame["agentState"])["live"]) {
		if here(row) {
			live = append(live, row)
		}
	}
	if len(live) == 0 {
		return ""
	}
	for _, row := range live {
		if row["isRunning"] == true {
			return ""
		}
	}
	return provider.WatchIdle
}

// Watch follows the session's transcript, starting after its newest entry as the SDK's watchBot does.
func (a *Adapter) Watch(ctx context.Context, credential string, s provider.SessionRef, event func(provider.WatchEvent) bool) error {
	bot, session := s.Ref["botId"], s.Ref["sessionId"]
	if !botID.MatchString(bot) {
		return nil
	}
	tail, err := a.data(ctx, credential, "ListGrokBotTranscriptEntries", map[string]any{"agentId": bot,
		"sessionId": session, "limit": 1})
	if err != nil {
		return err
	}
	body := ConnectFrame(map[string]any{
		"cursors": []any{map[string]any{"agentId": bot, "sessionId": session, "generation": generationOf(tail["generation"]),
			"afterUpdatedSeq": newestSeq(tail["entries"])}},
		"includeUnlistedAgents": false,
		"inlineBodyMaxBytes":    65_536,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.origin+"/aiserver.v1.GrokBotService/WatchGrokBotTranscripts",
		bytes.NewReader(body))
	if err != nil {
		return &provider.Error{Code: provider.CodeRequestFailed}
	}
	for k, v := range a.headers(credential) {
		req.Header.Set(k, v)
	}
	req.Header.Set("content-type", "application/connect+json")
	req.Header.Set("accept", "application/connect+json")
	req.Header.Set("connect-accept-encoding", "identity")
	resp, err := a.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return &provider.Error{Code: provider.CodeNetworkError, Retryable: true}
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return &provider.Error{Code: provider.CodeAuthFailed, HTTPStatus: resp.StatusCode}
	}
	media, _, _ := mime.ParseMediaType(resp.Header.Get("content-type"))
	if resp.StatusCode < 200 || resp.StatusCode > 299 || media != "application/connect+json" {
		return &provider.Error{Code: provider.CodeRequestFailed, HTTPStatus: resp.StatusCode}
	}
	return ReadConnectFrames(ctx, resp.Body, func(frame map[string]any) bool {
		if e := WatchEvent(frame, bot, session); e != "" {
			return event(e)
		}
		return true
	})
}
