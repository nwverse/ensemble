// Package serve is the standalone shell: club's protocol over HTTP, for the Lark gateway's agent bridge.
//
//	POST /v1/spaces/{space}/rpc     {"method": "agentsrc.status", "params": {...}} → {"result": ...} or {"error": {"code", "message"}}
//	GET  /v1/spaces/{space}/events  server-sent events: "agentsrc.updated" with {"session_keys"?: [...]}
//
// By default it listens on a unix socket only (mode 0600), where every caller is trusted as the socket's owner. A TCP
// listener needs tokens, at three levels: read, write and admin.
package serve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/nwverse/ensemble/internal/core"
	"github.com/nwverse/ensemble/internal/protocol"
)

// maxBody is the largest request body taken.
const maxBody = 1 << 20

// Server answers club's protocol over HTTP.
type Server struct {
	club *core.Club
	// tokens, when set, are required on every request; nil trusts every caller (the unix socket).
	tokens    *Tokens
	heartbeat time.Duration
}

// New is a server for club. tokens nil trusts every caller with admin: use it only on the unix socket.
func New(club *core.Club, tokens *Tokens) *Server {
	return &Server{club: club, tokens: tokens, heartbeat: 25 * time.Second}
}

// Handler is the HTTP handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/spaces/{space}/rpc", s.rpc)
	mux.HandleFunc("GET /v1/spaces/{space}/events", s.events)
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok\n") })
	return mux
}

type rpcRequest struct {
	Method string          `json:"method"`
	Params json.RawMessage `json:"params,omitempty"`
}

type rpcResponse struct {
	Result any             `json:"result,omitempty"`
	Error  *protocol.Error `json:"error,omitempty"`
}

func (s *Server) level(r *http.Request) Level {
	if s.tokens == nil {
		return LevelAdmin
	}
	tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		return LevelNone
	}
	return s.tokens.Level(strings.TrimSpace(tok))
}

// authorize answers 401 or 403 and returns false when the request may not do what needs level.
func (s *Server) authorize(w http.ResponseWriter, r *http.Request, need Level) bool {
	have := s.level(r)
	switch {
	case have == LevelNone:
		w.Header().Set("WWW-Authenticate", `Bearer realm="personal-agent-club"`)
		writeJSON(w, http.StatusUnauthorized, rpcResponse{Error: &protocol.Error{Code: "unauthorized", Message: "a valid token is required"}})
		return false
	case have < need:
		writeJSON(w, http.StatusForbidden, rpcResponse{Error: &protocol.Error{Code: "forbidden",
			Message: fmt.Sprintf("this needs a %s token", need)}})
		return false
	}
	return true
}

func (s *Server) rpc(w http.ResponseWriter, r *http.Request) {
	if s.level(r) == LevelNone {
		s.authorize(w, r, LevelRead)
		return
	}
	var req rpcRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody))
	if err := dec.Decode(&req); err != nil || req.Method == "" {
		writeJSON(w, http.StatusBadRequest, rpcResponse{Error: protocol.Invalid("the body must be {\"method\": ..., \"params\": ...}")})
		return
	}
	if !s.authorize(w, r, Required(req.Method)) {
		return
	}
	res, err := s.club.Call(r.Context(), r.PathValue("space"), req.Method, req.Params)
	if err != nil {
		var pe *protocol.Error
		if !errors.As(err, &pe) {
			pe = &protocol.Error{Message: err.Error()}
		}
		writeJSON(w, statusOf(pe.Code), rpcResponse{Error: pe})
		return
	}
	writeJSON(w, http.StatusOK, rpcResponse{Result: res})
}

// statusOf is the HTTP status of a protocol code, as the Persona hub maps them for its apps.
func statusOf(code string) int {
	switch code {
	case protocol.CodeNotFound:
		return http.StatusNotFound
	case protocol.CodeInvalid:
		return http.StatusBadRequest
	case protocol.CodeConflict:
		return http.StatusConflict
	case protocol.CodeUnsupported:
		return http.StatusNotImplemented
	}
	return http.StatusBadGateway
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	if !s.authorize(w, r, LevelRead) {
		return
	}
	space := r.PathValue("space")
	if _, err := s.club.Space(r.Context(), space); err != nil {
		var pe *protocol.Error
		if !errors.As(err, &pe) {
			pe = &protocol.Error{Message: err.Error()}
		}
		writeJSON(w, statusOf(pe.Code), rpcResponse{Error: pe})
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeJSON(w, http.StatusInternalServerError, rpcResponse{Error: &protocol.Error{Message: "streaming unsupported"}})
		return
	}
	sub := s.club.Subscribe(space)
	defer sub.Close()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, ": connected\n\n")
	flusher.Flush()
	tick := time.NewTicker(s.heartbeat)
	defer tick.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-tick.C:
			if _, err := io.WriteString(w, ": ping\n\n"); err != nil {
				return
			}
		case <-sub.Ready():
			for _, e := range sub.Take() {
				data, _ := json.Marshal(protocol.UpdatedEvent{SessionKeys: e.SessionKeys})
				if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", protocol.EventUpdated, data); err != nil {
					return
				}
			}
		}
		flusher.Flush()
	}
}

// Config is where to listen.
type Config struct {
	// Socket is a unix socket path.
	Socket string
	// Addr is a TCP address (host:port); it needs Tokens.
	Addr string
	// Tokens are required for Addr; on the socket too when set.
	Tokens *Tokens
}

// Check refuses a config that would listen without protection: TCP without tokens, or nothing to listen on.
func (c Config) Check() error {
	if c.Socket == "" && c.Addr == "" {
		return errors.New("nothing to listen on: give a unix socket or a TCP address")
	}
	if c.Addr != "" && c.Tokens == nil {
		return errors.New("listening on TCP needs a tokens file (read, write and admin tokens)")
	}
	return nil
}

// Listen opens the listeners of c.
func Listen(c Config) ([]net.Listener, error) {
	if err := c.Check(); err != nil {
		return nil, err
	}
	var ls []net.Listener
	if c.Socket != "" {
		l, err := listenUnix(c.Socket)
		if err != nil {
			return nil, err
		}
		ls = append(ls, l)
	}
	if c.Addr != "" {
		l, err := net.Listen("tcp", c.Addr)
		if err != nil {
			for _, x := range ls {
				_ = x.Close()
			}
			return nil, err
		}
		ls = append(ls, l)
	}
	return ls, nil
}

func listenUnix(path string) (net.Listener, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	if fi, err := os.Lstat(path); err == nil {
		if fi.Mode()&os.ModeSocket == 0 {
			return nil, fmt.Errorf("%s exists and is not a socket", path)
		}
		if conn, err := net.Dial("unix", path); err == nil {
			_ = conn.Close()
			return nil, fmt.Errorf("%s is in use by another process", path)
		}
		_ = os.Remove(path)
	}
	l, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = l.Close()
		return nil, err
	}
	return l, nil
}

// Run serves club on c's listeners until ctx ends.
func Run(ctx context.Context, club *core.Club, c Config) ([]net.Addr, <-chan error, error) {
	ls, err := Listen(c)
	if err != nil {
		return nil, nil, err
	}
	// Requests' contexts end with ctx, so event streams close on shutdown instead of holding it up.
	base, cancelBase := context.WithCancel(context.WithoutCancel(ctx))
	srv := &http.Server{Handler: New(club, c.Tokens).Handler(), ReadHeaderTimeout: 30 * time.Second,
		BaseContext: func(net.Listener) context.Context { return base }}
	errs := make(chan error, len(ls)+1)
	addrs := make([]net.Addr, len(ls))
	for i, l := range ls {
		addrs[i] = l.Addr()
		go func() {
			if err := srv.Serve(l); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errs <- err
			}
		}()
	}
	done := make(chan error, 1)
	go func() {
		var err error
		select {
		case <-ctx.Done():
		case err = <-errs:
		}
		cancelBase()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
		if c.Socket != "" {
			_ = os.Remove(c.Socket)
		}
		done <- err
	}()
	return addrs, done, nil
}
