// Package web is what the API providers share: one HTTP client that goes through the configured proxy, one JSON
// request that never follows a redirect, and readers for loosely typed JSON.
package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/nwverse/ensemble/internal/provider"
)

// MaxResponse is the largest response body read; a larger one is dropped.
const MaxResponse = 8 << 20

// DefaultTimeout bounds one request.
const DefaultTimeout = 20 * time.Second

var errRedirect = errors.New("redirect refused")

// NewClient is the HTTP client of every provider. proxy "" uses HTTP_PROXY / HTTPS_PROXY / NO_PROXY from the
// environment; otherwise every request goes through proxy (socks5://, socks5h://, http:// or https:// with a host and
// port), and fails when the proxy is down instead of going direct. Redirects are refused, so a credential header
// never follows one to another host.
func NewClient(proxy string) (*http.Client, error) {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.Proxy = http.ProxyFromEnvironment
	if proxy != "" {
		u, err := ParseProxy(proxy)
		if err != nil {
			return nil, err
		}
		t.Proxy = http.ProxyURL(u)
	}
	return &http.Client{Transport: t, CheckRedirect: func(*http.Request, []*http.Request) error { return errRedirect }}, nil
}

// UseClient is c as a provider client: a copy that refuses redirects, as NewClient's does. c's transport (and so
// its proxy) is kept: for a host that hands its own client (the Persona hub's egress proxy).
func UseClient(c *http.Client) *http.Client {
	out := *c
	out.CheckRedirect = func(*http.Request, []*http.Request) error { return errRedirect }
	return &out
}

// ParseProxy checks a proxy URL.
func ParseProxy(v string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(v))
	if err != nil || u.Hostname() == "" || u.Port() == "" {
		return nil, errors.New("the proxy must be a socks5://, socks5h://, http:// or https:// URL with a host and port")
	}
	switch u.Scheme {
	case "socks5", "socks5h", "http", "https":
		return u, nil
	}
	return nil, errors.New("the proxy must be a socks5://, socks5h://, http:// or https:// URL with a host and port")
}

// Result is one JSON request's outcome. Transport is true when no answer came (or it could not be read).
type Result struct {
	OK        bool
	Status    int
	Data      any
	Header    http.Header
	Transport bool
}

// Request is one JSON request. body nil sends none. A body over MaxResponse, or one that is not JSON, is a failed
// result that is not a transport failure.
func Request(ctx context.Context, c *http.Client, method, target string, headers map[string]string, body any,
	timeout time.Duration) Result {
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return Result{Transport: true}
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, rd)
	if err != nil {
		return Result{Transport: true}
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := c.Do(req)
	if err != nil {
		return Result{Transport: true}
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, MaxResponse+1))
	if err != nil {
		return Result{Status: resp.StatusCode, Header: resp.Header, Transport: true}
	}
	if len(raw) > MaxResponse {
		return Result{Status: resp.StatusCode, Header: resp.Header}
	}
	var data any = map[string]any{}
	if len(bytes.TrimSpace(raw)) > 0 {
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.UseNumber()
		if dec.Decode(&data) != nil {
			return Result{Status: resp.StatusCode, Header: resp.Header}
		}
	}
	ok := resp.StatusCode >= 200 && resp.StatusCode < 300
	return Result{OK: ok, Status: resp.StatusCode, Data: data, Header: resp.Header}
}

// ReadError is the error a failed read becomes: auth failures and rate limits are named.
func ReadError(r Result) *provider.Error {
	switch {
	case r.Transport:
		return &provider.Error{Code: provider.CodeNetworkError, Retryable: true}
	case r.Status == http.StatusUnauthorized || r.Status == http.StatusForbidden:
		return &provider.Error{Code: provider.CodeAuthFailed, HTTPStatus: r.Status}
	case r.Status == http.StatusTooManyRequests:
		return &provider.Error{Code: provider.CodeRateLimited, HTTPStatus: r.Status, Retryable: true,
			RetryAfter: RetryAfter(r.Header, time.Now())}
	case r.Status >= 500:
		return &provider.Error{Code: provider.CodeUpstreamError, HTTPStatus: r.Status, Retryable: true}
	}
	return &provider.Error{Code: provider.CodeRequestFailed, HTTPStatus: r.Status}
}

// RetryAfter reads a Retry-After header: seconds or an HTTP date; 0 when absent or unreadable.
func RetryAfter(h http.Header, now time.Time) time.Duration {
	v := strings.TrimSpace(h.Get("Retry-After"))
	if v == "" {
		return 0
	}
	if n, err := strconv.ParseFloat(v, 64); err == nil && n > 0 && n < 1e7 {
		return time.Duration(n * float64(time.Second))
	}
	if t, err := http.ParseTime(v); err == nil && t.After(now) {
		return t.Sub(now)
	}
	return 0
}

// Record is v as a JSON object, or an empty one.
func Record(v any) map[string]any {
	if m, ok := v.(map[string]any); ok {
		return m
	}
	return map[string]any{}
}

// Records are the objects of a JSON array; anything else is skipped.
func Records(v any) []map[string]any {
	arr, _ := v.([]any)
	out := make([]map[string]any, 0, len(arr))
	for _, x := range arr {
		if m, ok := x.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

// IsArray says whether v is a JSON array.
func IsArray(v any) bool {
	_, ok := v.([]any)
	return ok
}

// Text is v as a string, or "".
func Text(v any) string {
	s, _ := v.(string)
	return s
}

// Strings are the strings of a JSON array.
func Strings(v any) []string {
	var out []string
	for _, x := range anySlice(v) {
		if s, ok := x.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func anySlice(v any) []any {
	arr, _ := v.([]any)
	return arr
}

const maxSafeInteger = 1<<53 - 1

// Integer is v as a safe integer (a JSON number without fraction), ok false otherwise.
func Integer(v any) (int64, bool) {
	switch n := v.(type) {
	case json.Number:
		i, err := strconv.ParseInt(string(n), 10, 64)
		if err != nil || i > maxSafeInteger || i < -maxSafeInteger {
			return 0, false
		}
		return i, true
	case float64:
		if n != float64(int64(n)) || n > maxSafeInteger || n < -maxSafeInteger {
			return 0, false
		}
		return int64(n), true
	}
	return 0, false
}

// Millis is a decimal string (up to 16 digits) or a safe integer number of milliseconds; anything else, or not
// positive, is 0.
func Millis(v any) int64 {
	var n int64
	switch x := v.(type) {
	case string:
		if len(x) == 0 || len(x) > 16 || strings.Trim(x, "0123456789") != "" {
			return 0
		}
		n, _ = strconv.ParseInt(x, 10, 64)
	default:
		n, _ = Integer(v)
	}
	return max(n, 0)
}

// Is says whether v is the JSON value want (a string, or an integer).
func Is(v any, want ...any) bool {
	for _, w := range want {
		switch w := w.(type) {
		case string:
			if s, ok := v.(string); ok && s == w {
				return true
			}
		case int:
			if n, ok := Integer(v); ok && n == int64(w) {
				return true
			}
		}
	}
	return false
}

// String is how JavaScript's String(v) writes a JSON scalar (null and undefined become "").
func String(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case json.Number:
		return x.String()
	case bool:
		return strconv.FormatBool(x)
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	}
	b, _ := json.Marshal(v)
	return fmt.Sprint(string(b))
}
