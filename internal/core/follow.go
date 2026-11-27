package core

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/nwverse/ensemble/internal/protocol"
	"github.com/nwverse/ensemble/internal/store"
)

// FollowLease TTL presets and limits.
const (
	FollowTTL2h        = "2h"
	FollowTTL6h        = "6h"
	FollowTTL24h       = "24h"
	MinFollowTTLMs     = 60_000
	DefaultFollowTTLMs = 2 * 3_600_000
	MaxFollowTTLMs     = 24 * 3_600_000

	DefaultFollowMaxTurns = 10
	MaxFollowMaxTurns     = 50
	maxLeaseNoteChars     = 200
)

// FollowTurnsPresets are the standard turn caps offered in the UI.
var FollowTurnsPresets = []int{10, 20, 50}

// FollowTTLPresets are the standard TTL presets offered in the UI.
var FollowTTLPresets = []string{FollowTTL2h, FollowTTL6h, FollowTTL24h}

// Burst rate-limit constants for assistant writes per session (or per account for session creation).
const (
	BurstWindowMs  = 60_000
	BurstMaxWrites = 5
	maxAuditLog    = 256
)

// NewLeaseID generates a random follow-lease identifier ("fl_" + 12 hex digits).
func NewLeaseID() string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return "fl_" + hex.EncodeToString(b[:])
}

// ParseFollowTTL resolves a TTL preset string ("2h", "6h", "24h", or a Go duration string) or an explicit ttlMs into
// milliseconds, bounded to [MinFollowTTLMs, MaxFollowTTLMs]. When both are empty/zero, DefaultFollowTTLMs is returned.
func ParseFollowTTL(ttl string, ttlMs int64) (int64, error) {
	ttl = strings.TrimSpace(ttl)
	if ttl != "" {
		switch ttl {
		case FollowTTL2h:
			return 2 * 3_600_000, nil
		case FollowTTL6h:
			return 6 * 3_600_000, nil
		case FollowTTL24h:
			return 24 * 3_600_000, nil
		}
		d, err := time.ParseDuration(ttl)
		if err != nil {
			return 0, badInput(fmt.Sprintf("invalid follow lease ttl %q (use 2h, 6h or 24h)", ttl))
		}
		ms := d.Milliseconds()
		if ms < MinFollowTTLMs || ms > MaxFollowTTLMs {
			return 0, badInput("follow lease ttl must be between 1m and 24h")
		}
		return ms, nil
	}
	if ttlMs != 0 {
		if ttlMs < MinFollowTTLMs || ttlMs > MaxFollowTTLMs {
			return 0, badInput("follow lease ttl_ms must be between 60000 and 86400000")
		}
		return ttlMs, nil
	}
	return DefaultFollowTTLMs, nil
}

// NormalizeFollowMaxTurns validates or defaults a lease's max_turns, bounded to [1, MaxFollowMaxTurns].
func NormalizeFollowMaxTurns(maxTurns int) (int, error) {
	if maxTurns == 0 {
		return DefaultFollowMaxTurns, nil
	}
	if maxTurns < 1 || maxTurns > MaxFollowMaxTurns {
		return 0, badInput(fmt.Sprintf("max_turns must be between 1 and %d", MaxFollowMaxTurns))
	}
	return maxTurns, nil
}

func followLeaseView(l store.FollowLease, sessionTitle string, now int64) protocol.FollowLease {
	return protocol.FollowLease{
		LeaseID:         l.LeaseID,
		AccountID:       l.AccountID,
		SessionKey:      l.SessionKey,
		SessionTitle:    sessionTitle,
		Status:          l.Status(now),
		CreatedAt:       l.CreatedAt,
		ExpiresAt:       l.ExpiresAt,
		MaxTurns:        l.MaxTurns,
		UsedTurns:       l.UsedTurns,
		RemainingTurns:  l.RemainingTurns(),
		AllowBackground: l.AllowBackground,
		RevokedAt:       l.RevokedAt,
		RevokeReason:    l.RevokeReason,
		Note:            l.Note,
	}
}

// GrantFollowLease creates (or replaces) a session-scoped follow lease. Owner-only (OpManage).
func (s *Space) GrantFollowLease(ctx context.Context, p protocol.GrantFollowLeaseParams) (protocol.FollowLease, error) {
	sess, err := s.requireSession(ctx, strings.TrimSpace(p.SessionKey))
	if err != nil {
		return protocol.FollowLease{}, err
	}
	if !sess.CanSend {
		return protocol.FollowLease{}, protocol.Conflict("this session is read-only").Because(protocol.ReasonSessionReadOnly)
	}
	if active, err := s.AssistantSessionKeys(ctx); err == nil && active[sess.SessionKey] {
		return protocol.FollowLease{}, badInput("cannot grant follow lease on active main-assistant backing session")
	}
	if p.AccountID != "" && p.AccountID != sess.AccountID {
		return protocol.FollowLease{}, protocol.Conflict("session does not belong to account").Because(protocol.ReasonAccountMismatch)
	}
	acct, err := s.requireAccount(ctx, sess.AccountID)
	if err != nil {
		return protocol.FollowLease{}, err
	}
	ttlMs, err := ParseFollowTTL(p.TTL, p.TTLMs)
	if err != nil {
		return protocol.FollowLease{}, err
	}
	maxTurns, err := NormalizeFollowMaxTurns(p.MaxTurns)
	if err != nil {
		return protocol.FollowLease{}, err
	}
	note := strings.TrimSpace(p.Note)
	if len(utf16.Encode([]rune(note))) > maxLeaseNoteChars {
		return protocol.FollowLease{}, badInput("note is longer than 200 characters")
	}
	now := s.now()
	row := store.FollowLease{
		LeaseID:         NewLeaseID(),
		AccountID:       acct.AccountID,
		SessionKey:      sess.SessionKey,
		CreatedAt:       now,
		ExpiresAt:       now + ttlMs,
		MaxTurns:        maxTurns,
		UsedTurns:       0,
		AllowBackground: p.AllowBackground,
		Note:            note,
		UpdatedAt:       now,
	}
	if err := s.store.PutFollowLease(ctx, row); err != nil {
		return protocol.FollowLease{}, err
	}
	ctrl, _ := ParseControl(acct.AssistantControl)
	s.RecordAudit(AuditEntry{
		TimeMs:     now,
		AccountID:  acct.AccountID,
		SessionKey: sess.SessionKey,
		Tool:       protocol.MethodClubLeasesGrant,
		Operation:  OpManage,
		Level:      ctrl,
		Verdict:    Allow,
		LeaseID:    row.LeaseID,
		LeaseUsed:  0,
		LeaseMax:   row.MaxTurns,
		Reason:     "owner_grant_lease",
	})
	s.notify([]string{sess.SessionKey})
	return followLeaseView(row, sess.Title, now), nil
}

// ListFollowLeases lists follow leases for an account (all or active-only), newest first.
func (s *Space) ListFollowLeases(ctx context.Context, p protocol.ListFollowLeasesParams) (protocol.ListFollowLeasesResult, error) {
	if _, err := s.requireAccount(ctx, strings.TrimSpace(p.AccountID)); err != nil {
		return protocol.ListFollowLeasesResult{}, err
	}
	rows, err := s.store.AccountFollowLeases(ctx, p.AccountID)
	if err != nil {
		return protocol.ListFollowLeasesResult{}, err
	}
	sessions, _ := s.store.AccountSessions(ctx, p.AccountID)
	titles := make(map[string]string, len(sessions))
	for _, sess := range sessions {
		titles[sess.SessionKey] = sess.Title
	}
	now := s.now()
	out := make([]protocol.FollowLease, 0, len(rows))
	for _, r := range rows {
		if p.ActiveOnly && !r.IsActive(now) {
			continue
		}
		out = append(out, followLeaseView(r, titles[r.SessionKey], now))
	}
	return protocol.ListFollowLeasesResult{Leases: out}, nil
}

// ActiveFollowLease returns the active follow lease for sessionKey at the current time, if one exists.
func (s *Space) ActiveFollowLease(ctx context.Context, sessionKey string) (store.FollowLease, bool, error) {
	if strings.TrimSpace(sessionKey) == "" {
		return store.FollowLease{}, false, nil
	}
	return s.store.ActiveFollowLease(ctx, sessionKey, s.now())
}

type consumedLeaseEntry struct {
	leaseID string
	at      int64
}

// ConsumeFollowLease atomically consumes one turn from the active lease of sessionKey at the current time. When
// requestID is non-empty and was already consumed, the existing lease state is returned without double-decrementing.
func (s *Space) ConsumeFollowLease(ctx context.Context, sessionKey string, unattended bool, requestID string) (store.FollowLease, bool, error) {
	now := s.now()
	s.mu.Lock()
	if requestID != "" {
		if s.consumedReqLeases == nil {
			s.consumedReqLeases = map[string]consumedLeaseEntry{}
		}
		for k, e := range s.consumedReqLeases {
			if now-e.at > localEchoWindowMs {
				delete(s.consumedReqLeases, k)
			}
		}
		if entry, ok := s.consumedReqLeases[requestID]; ok {
			s.mu.Unlock()
			l, found, err := s.store.FollowLease(ctx, entry.leaseID)
			return l, found, err
		}
	}
	row, ok, err := s.store.ConsumeFollowLease(ctx, sessionKey, unattended, now)
	if err != nil || !ok {
		s.mu.Unlock()
		return store.FollowLease{}, false, err
	}
	if requestID != "" {
		s.consumedReqLeases[requestID] = consumedLeaseEntry{leaseID: row.LeaseID, at: now}
	}
	s.mu.Unlock()
	s.log().Debug("follow lease turn consumed", "space", s.name, "account", row.AccountID, "session", row.SessionKey,
		"lease", row.LeaseID, "used_turns", row.UsedTurns, "max_turns", row.MaxTurns, "unattended", unattended)
	s.notify([]string{sessionKey})
	return row, true, nil
}

// RevokeFollowLease revokes one follow lease by lease_id or all active leases for an account_id. Owner-only (OpManage).
func (s *Space) RevokeFollowLease(ctx context.Context, p protocol.RevokeFollowLeaseParams) (protocol.RevokeFollowLeaseResult, error) {
	leaseID := strings.TrimSpace(p.LeaseID)
	accountID := strings.TrimSpace(p.AccountID)
	reason := strings.TrimSpace(p.Reason)
	if reason == "" {
		reason = "manual"
	}
	now := s.now()
	if leaseID != "" {
		cur, ok, err := s.store.FollowLease(ctx, leaseID)
		if err != nil {
			return protocol.RevokeFollowLeaseResult{}, err
		}
		if !ok {
			return protocol.RevokeFollowLeaseResult{}, protocol.NotFound("no such follow lease").Because(protocol.ReasonLeaseNotFound)
		}
		if accountID != "" && cur.AccountID != accountID {
			return protocol.RevokeFollowLeaseResult{}, protocol.Conflict("lease does not belong to account").Because(protocol.ReasonAccountMismatch)
		}
		row, changed, err := s.store.RevokeFollowLease(ctx, leaseID, reason, now)
		if err != nil {
			return protocol.RevokeFollowLeaseResult{}, err
		}
		if !changed {
			return protocol.RevokeFollowLeaseResult{}, protocol.NotFound("follow lease is already revoked").Because(protocol.ReasonLeaseNotFound)
		}
		var title string
		if sess, ok, _ := s.store.Session(ctx, row.SessionKey); ok {
			title = sess.Title
		}
		s.RecordAudit(AuditEntry{
			TimeMs:     now,
			AccountID:  row.AccountID,
			SessionKey: row.SessionKey,
			Tool:       protocol.MethodClubLeasesRevoke,
			Operation:  OpManage,
			Verdict:    Allow,
			LeaseID:    row.LeaseID,
			LeaseUsed:  row.UsedTurns,
			LeaseMax:   row.MaxTurns,
			Reason:     "owner_revoke_lease:" + reason,
		})
		s.notify([]string{row.SessionKey})
		v := followLeaseView(row, title, now)
		return protocol.RevokeFollowLeaseResult{Revoked: 1, Lease: &v}, nil
	}
	if accountID != "" {
		if _, err := s.requireAccount(ctx, accountID); err != nil {
			return protocol.RevokeFollowLeaseResult{}, err
		}
		n, err := s.store.RevokeAccountFollowLeases(ctx, accountID, reason, now)
		if err != nil {
			return protocol.RevokeFollowLeaseResult{}, err
		}
		s.RecordAudit(AuditEntry{
			TimeMs:    now,
			AccountID: accountID,
			Tool:      protocol.MethodClubLeasesRevoke,
			Operation: OpManage,
			Verdict:   Allow,
			Reason:    fmt.Sprintf("owner_revoke_account_leases:%s(%d)", reason, n),
		})
		if n > 0 {
			s.notify(nil)
		}
		return protocol.RevokeFollowLeaseResult{Revoked: n}, nil
	}
	return protocol.RevokeFollowLeaseResult{}, badInput("lease_id or account_id is required")
}

// HasUnconfirmedSend checks whether sessionKey currently has an unconfirmed local send (LocalUnconfirmed) that has not
// been echoed by a remote user message within localEchoWindowMs.
func (s *Space) HasUnconfirmedSend(ctx context.Context, sessionKey string) (store.Outbox, bool, error) {
	rows, err := s.store.LocalMessages(ctx, sessionKey, s.now()-localEchoWindowMs)
	if err != nil {
		return store.Outbox{}, false, err
	}
	for _, r := range rows {
		if r.Status == protocol.LocalUnconfirmed {
			return r, true, nil
		}
	}
	return store.Outbox{}, false, nil
}

// CheckBurstLimit performs a read-only check of whether targetKey (a session_key, or "start:<account_id>") has reached
// BurstMaxWrites within the last BurstWindowMs. When allowed is false, retryAfterMs is how long until the oldest
// recorded write in the window expires.
func (s *Space) CheckBurstLimit(targetKey string) (allowed bool, retryAfterMs int64) {
	if targetKey == "" {
		return true, 0
	}
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	times := s.pruneBurstLocked(targetKey, now)
	if len(times) >= BurstMaxWrites {
		wait := max(1, (times[0]+BurstWindowMs)-now)
		return false, wait
	}
	return true, 0
}

// RecordBurstWrite checks and records one assistant write against targetKey's burst window. Retries with the same
// non-empty requestID are idempotent and do not consume an additional burst slot.
func (s *Space) RecordBurstWrite(targetKey, requestID string) (allowed bool, retryAfterMs int64) {
	if targetKey == "" {
		return true, 0
	}
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	if requestID != "" {
		if s.burstReqSeen == nil {
			s.burstReqSeen = map[string]int64{}
		}
		for k, at := range s.burstReqSeen {
			if now-at > BurstWindowMs*2 {
				delete(s.burstReqSeen, k)
			}
		}
		if _, seen := s.burstReqSeen[requestID]; seen {
			return true, 0
		}
	}
	times := s.pruneBurstLocked(targetKey, now)
	if len(times) >= BurstMaxWrites {
		wait := max(1, (times[0]+BurstWindowMs)-now)
		return false, wait
	}
	if s.burstWrites == nil {
		s.burstWrites = map[string][]int64{}
	}
	s.burstWrites[targetKey] = append(times, now)
	if requestID != "" {
		s.burstReqSeen[requestID] = now
	}
	return true, 0
}

func (s *Space) pruneBurstLocked(targetKey string, now int64) []int64 {
	if s.burstWrites == nil {
		return nil
	}
	cutoff := now - BurstWindowMs
	times := s.burstWrites[targetKey]
	idx := 0
	for idx < len(times) && times[idx] <= cutoff {
		idx++
	}
	if idx > 0 {
		times = slices.Clone(times[idx:])
		if len(times) == 0 {
			delete(s.burstWrites, targetKey)
		} else {
			s.burstWrites[targetKey] = times
		}
	}
	return times
}

// AuditEntry is one structured permission & safety-guard audit record. It never contains credentials, tokens, cookies
// or message bodies.
type AuditEntry struct {
	TimeMs     int64
	AccountID  string
	SessionKey string
	Tool       string
	Operation  Operation
	Level      Control
	Unattended bool
	Verdict    Verdict
	LeaseID    string
	LeaseUsed  int
	LeaseMax   int
	Bypass     bool
	Guard      string
	Reason     string
}

// RecordAudit logs and retains a structured permission decision or safety-guard event.
func (s *Space) RecordAudit(e AuditEntry) {
	if e.TimeMs == 0 {
		e.TimeMs = s.now()
	}
	s.mu.Lock()
	if len(s.audits) >= maxAuditLog {
		s.audits = append(s.audits[:0], s.audits[1:]...)
	}
	s.audits = append(s.audits, e)
	s.mu.Unlock()

	attrs := []any{
		"space", s.name,
		"account", e.AccountID,
		"session", e.SessionKey,
		"tool", e.Tool,
		"op", string(e.Operation),
		"level", string(e.Level),
		"unattended", e.Unattended,
		"verdict", string(e.Verdict),
		"bypass", e.Bypass,
	}
	if e.LeaseID != "" {
		attrs = append(attrs, "lease", e.LeaseID, "lease_used", e.LeaseUsed, "lease_max", e.LeaseMax)
	}
	if e.Guard != "" {
		attrs = append(attrs, "guard", e.Guard)
	}
	if e.Reason != "" {
		attrs = append(attrs, "reason", e.Reason)
	}
	if e.Verdict == Deny || e.Guard != "" {
		s.log().Warn("permission audit", attrs...)
	} else {
		s.log().Debug("permission audit", attrs...)
	}
}

// AuditEntries returns a snapshot of recent permission audit records in chronological order.
func (s *Space) AuditEntries() []AuditEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.audits)
}
