// Package runtime bridges personal-agent-club into Persona's ~/.persona/runtime.sock assistant runtime protocol
// (docs/integrations/runtimes.md "Assistant"), letting paired club accounts serve as Persona's main assistant.
package runtime

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

// ProtocolVersion matches Persona node's RuntimeProtocol (server/internal/node/runtimes.go).
const ProtocolVersion = 1

// RPCError carries an optional Persona wire error code and JSON data payload.
type RPCError struct {
	Message string          `json:"message"`
	Code    string          `json:"code,omitempty"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *RPCError) Error() string {
	if e.Code != "" {
		return e.Code + ": " + e.Message
	}
	return e.Message
}

type wireFrame struct {
	ID     string          `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Event  string          `json:"event,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *RPCError       `json:"error,omitempty"`
}

// Peer implements Persona's bidirectional newline-delimited JSON RPC protocol (server/internal/rpc).
type Peer struct {
	prefix string
	rwc    io.ReadWriteCloser

	OnRequest func(ctx context.Context, method string, params json.RawMessage) (any, error)
	OnEvent   func(event string, params json.RawMessage)

	next atomic.Int64
	wmu  sync.Mutex

	mu       sync.Mutex
	closed   bool
	closeErr error
	pending  map[string]chan wireFrame
	inflight map[string]context.CancelFunc
}

// NewPeer wraps an open stream in a Persona JSON-lines RPC Peer.
func NewPeer(prefix string, rwc io.ReadWriteCloser) *Peer {
	if prefix == "" {
		prefix = "r"
	}
	return &Peer{
		prefix:   prefix,
		rwc:      rwc,
		pending:  map[string]chan wireFrame{},
		inflight: map[string]context.CancelFunc{},
	}
}

func (p *Peer) writeFrame(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	p.wmu.Lock()
	defer p.wmu.Unlock()
	p.mu.Lock()
	closed := p.closed
	p.mu.Unlock()
	if closed {
		return io.ErrClosedPipe
	}
	_, err = p.rwc.Write(b)
	return err
}

// Call invokes method on the remote peer and decodes its result into out when out != nil.
func (p *Peer) Call(ctx context.Context, method string, params, out any) error {
	id := p.prefix + strconv.FormatInt(p.next.Add(1), 10)
	var rawParams json.RawMessage
	if params != nil {
		var err error
		rawParams, err = json.Marshal(params)
		if err != nil {
			return err
		}
	}
	ch := make(chan wireFrame, 1)
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return io.ErrClosedPipe
	}
	p.pending[id] = ch
	p.mu.Unlock()

	if err := p.writeFrame(wireFrame{ID: id, Method: method, Params: rawParams}); err != nil {
		p.mu.Lock()
		delete(p.pending, id)
		p.mu.Unlock()
		return err
	}

	select {
	case f, ok := <-ch:
		if !ok {
			return io.ErrClosedPipe
		}
		if f.Error != nil {
			return f.Error
		}
		if out != nil && len(f.Result) > 0 && string(f.Result) != "null" {
			return json.Unmarshal(f.Result, out)
		}
		return nil
	case <-ctx.Done():
		p.mu.Lock()
		delete(p.pending, id)
		p.mu.Unlock()
		_ = p.Notify("rpc.cancel", map[string]string{"id": id})
		return ctx.Err()
	}
}

// Notify sends a one-way event to the remote peer.
func (p *Peer) Notify(event string, params any) error {
	var raw json.RawMessage
	if params != nil {
		var err error
		raw, err = json.Marshal(params)
		if err != nil {
			return err
		}
	}
	return p.writeFrame(wireFrame{Event: event, Params: raw})
}

// Close closes the underlying connection and releases all waiting callers.
func (p *Peer) Close(err error) {
	if err == nil {
		err = io.ErrClosedPipe
	}
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	p.closeErr = err
	for id, ch := range p.pending {
		close(ch)
		delete(p.pending, id)
	}
	for id, cancel := range p.inflight {
		cancel()
		delete(p.inflight, id)
	}
	p.mu.Unlock()
	_ = p.rwc.Close()
}

// Serve reads frames from the remote peer until EOF or ctx cancellation.
func (p *Peer) Serve(ctx context.Context) error {
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			p.Close(ctx.Err())
		case <-done:
		}
	}()

	sc := bufio.NewScanner(p.rwc)
	sc.Buffer(make([]byte, 64*1024), 16<<20)
	var wg sync.WaitGroup
	defer wg.Wait()
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var f wireFrame
		if json.Unmarshal([]byte(line), &f) != nil {
			continue
		}
		switch {
		case f.Event == "rpc.cancel":
			var target struct {
				ID string `json:"id"`
			}
			if json.Unmarshal(f.Params, &target) == nil && target.ID != "" {
				p.mu.Lock()
				cancel := p.inflight[target.ID]
				p.mu.Unlock()
				if cancel != nil {
					cancel()
				}
			}
		case f.Event != "":
			if p.OnEvent != nil {
				p.OnEvent(f.Event, f.Params)
			}
		case f.Method != "":
			reqID, method, params := f.ID, f.Method, f.Params
			wg.Add(1)
			go func() {
				defer wg.Done()
				p.handleRequest(ctx, reqID, method, params)
			}()
		case f.ID != "":
			p.mu.Lock()
			ch := p.pending[f.ID]
			delete(p.pending, f.ID)
			p.mu.Unlock()
			if ch != nil {
				ch <- f
			}
		}
	}
	err := sc.Err()
	if err == nil {
		err = io.EOF
	}
	p.Close(err)
	return err
}

func (p *Peer) handleRequest(parent context.Context, id, method string, params json.RawMessage) {
	ctx, cancel := context.WithCancel(parent)
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		cancel()
		return
	}
	p.inflight[id] = cancel
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		delete(p.inflight, id)
		p.mu.Unlock()
		cancel()
	}()

	if p.OnRequest == nil {
		_ = p.writeFrame(wireFrame{ID: id, Error: &RPCError{Code: "unsupported", Message: "unknown method " + method}})
		return
	}
	res, err := p.OnRequest(ctx, method, params)
	if err != nil {
		var re *RPCError
		if !errors.As(err, &re) {
			re = &RPCError{Message: err.Error()}
		}
		_ = p.writeFrame(wireFrame{ID: id, Error: re})
		return
	}
	raw, mErr := json.Marshal(res)
	if mErr != nil {
		_ = p.writeFrame(wireFrame{ID: id, Error: &RPCError{Message: mErr.Error()}})
		return
	}
	if len(raw) == 0 || string(raw) == "null" {
		raw = json.RawMessage("{}")
	}
	_ = p.writeFrame(wireFrame{ID: id, Result: raw})
}

// DefaultPersonaSocketPath returns $PERSONA_RUNTIME_SOCKET or ~/.persona/runtime.sock.
func DefaultPersonaSocketPath() string {
	if v := strings.TrimSpace(os.Getenv("PERSONA_RUNTIME_SOCKET")); v != "" {
		return v
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".persona", "runtime.sock")
}

func randomUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
