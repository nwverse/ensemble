package core

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/nwverse/ensemble/internal/names"
	"github.com/nwverse/ensemble/internal/protocol"
	"github.com/nwverse/ensemble/internal/provider"
	"github.com/nwverse/ensemble/internal/store"
)

// Quota comes from provider quota APIs/headers (Groq rate-limit headers, Grok Bot GetSandUsageStatus, Claude Duet
// /usage, ChatGPT /wham/usage) and any 429 response. Readings are kept in memory; local activity estimates (5h/24h
// send counts) are derived from the store when needed.
const (
	quotaLowAt          = 0.2
	quotaStaleMs        = 15 * 60_000
	quotaUnknownReset   = 5 * 60_000
	quotaPollIntervalMs = 5 * 60_000
)

type quotaState struct {
	readings  []provider.QuotaReading
	checkedAt int64
	// limitedUntil is when a 429 stops counting as exhausted; resetsAt is what the provider said (0 if nothing).
	limitedUntil int64
	resetsAt     int64
	// announced is the state and staleness clients were last told about (quotaKey), under Space.mu.
	announced string
}

// observe records a 429 among err.
func (s *Space) observe(accountID string, err error) {
	var pe *provider.Error
	if accountID == "" || !errors.As(err, &pe) || (pe.HTTPStatus != 429 && pe.Code != provider.CodeRateLimited) {
		return
	}
	now := s.now()
	q := &quotaState{checkedAt: now, limitedUntil: now + quotaUnknownReset}
	if pe.RetryAfter > 0 {
		q.resetsAt = now + pe.RetryAfter.Milliseconds()
		q.limitedUntil = q.resetsAt
	}
	s.setQuota(accountID, q)
}

// noteQuota records an answer's readings.
func (s *Space) noteQuota(accountID string, readings []provider.QuotaReading) {
	if accountID == "" || len(readings) == 0 {
		return
	}
	s.setQuota(accountID, &quotaState{readings: readings, checkedAt: s.now()})
}

// setQuota keeps an account's new quota. Clients hear of it (agentsrc.updated, the list) only when what they draw
// changes: the state, or whether it is stale; and again, without a new answer, when time alone changes that (the
// reading goes stale, a 429's wait ends, a window resets).
func (s *Space) setQuota(accountID string, q *quotaState) {
	now := s.now()
	key := quotaKey(q, now)
	s.mu.Lock()
	before := ""
	if old := s.quota[accountID]; old != nil {
		before = old.announced
	}
	q.announced = key
	s.quota[accountID] = q
	s.mu.Unlock()
	if key != before {
		s.notify(nil)
	}
	for _, at := range quotaChanges(q) {
		if at > now {
			s.club.after(time.Duration(at-now)*time.Millisecond, func() { s.recheckQuota(accountID, q) })
		}
	}
}

// recheckQuota announces q again if time alone changed what clients draw and q is still the account's quota.
func (s *Space) recheckQuota(accountID string, q *quotaState) {
	key := quotaKey(q, s.now())
	s.mu.Lock()
	changed := s.quota[accountID] == q && q.announced != key
	if changed {
		q.announced = key
	}
	s.mu.Unlock()
	if changed {
		s.notify(nil)
	}
}

// quotaChanges are the moments (Unix ms) at which q's view changes with no new answer.
func quotaChanges(q *quotaState) []int64 {
	out := []int64{q.checkedAt + quotaStaleMs + 1}
	if q.limitedUntil != 0 {
		out = append(out, q.limitedUntil)
	}
	for _, r := range q.readings {
		if r.ResetsAt != 0 {
			out = append(out, r.ResetsAt)
		}
	}
	return out
}

// quotaKey is what a client draws of q: its state ("" unknown) and whether it is stale.
func quotaKey(q *quotaState, now int64) string {
	v := buildQuota(q, false, "", now)
	if v == nil {
		return ""
	}
	if v.Stale {
		return v.State + "/stale"
	}
	return v.State
}

func (s *Space) quotaView(a store.Account, now int64) *protocol.Quota {
	s.mu.Lock()
	q := s.quota[a.AccountID]
	s.mu.Unlock()
	return buildQuota(q, a.ShowQuota, a.QuotaDisplay, now)
}

func quotaBucketID(r provider.QuotaReading) string {
	if r.Window != "" {
		return r.Kind + ":" + r.Window
	}
	return r.Kind
}

func quotaTone(state string, stale bool) string {
	if stale {
		return protocol.QuotaToneStale
	}
	switch state {
	case protocol.QuotaExhausted:
		return protocol.QuotaToneExhausted
	case protocol.QuotaLow:
		return protocol.QuotaToneLow
	default:
		return protocol.QuotaToneOK
	}
}

func buildQuota(q *quotaState, show bool, display string, now int64) *protocol.Quota {
	if q == nil || q.checkedAt == 0 {
		return nil
	}
	if display == protocol.QuotaDisplayOff {
		show = false
	}
	stale := now-q.checkedAt > quotaStaleMs
	if q.limitedUntil != 0 {
		if now >= q.limitedUntil {
			return nil
		}
		label := "exhausted"
		if q.resetsAt != 0 {
			label = "exhausted until " + time.UnixMilli(q.resetsAt).UTC().Format(time.RFC3339)
		}
		pct := 0
		return &protocol.Quota{State: protocol.QuotaExhausted, Percent: &pct, Tone: quotaTone(protocol.QuotaExhausted, stale),
			Label: label, ResetsAt: q.resetsAt, CheckedAt: q.checkedAt,
			Source: protocol.QuotaSourceResponse, Stale: stale, Show: show}
	}
	var dims []protocol.Quota
	var buckets []protocol.QuotaBucket
	for _, r := range q.readings {
		if r.Limit <= 0 {
			continue
		}
		left := math.Max(0, math.Min(1, r.Remaining/r.Limit))
		if r.Exhausted {
			left = 0
		}
		if r.ResetsAt != 0 && now >= r.ResetsAt {
			continue
		}
		left = math.Round(left*1000) / 1000
		pct := int(math.Round(left * 100))
		state := protocol.QuotaOK
		switch {
		case r.Exhausted || left == 0:
			state = protocol.QuotaExhausted
		case left <= quotaLowAt:
			state = protocol.QuotaLow
		}
		tone := quotaTone(state, stale)
		label := fmt.Sprintf("%.0f%% of %s left", left*100, r.Kind)
		if r.Window != "" {
			label += " (per " + r.Window + ")"
		}
		bid := quotaBucketID(r)
		rem := left
		pctCopy := pct
		dim := protocol.Quota{State: state, Remaining: &rem, Percent: &pctCopy, Tone: tone, Kind: r.Kind,
			Label: label, Selected: bid, ResetsAt: r.ResetsAt,
			CheckedAt: q.checkedAt, Source: protocol.QuotaSourceProvider, SourceNote: r.SourceNote, Stale: stale, Show: show}
		bucket := protocol.QuotaBucket{ID: bid, Kind: r.Kind, Window: r.Window, Label: label,
			Remaining: &rem, Percent: &pctCopy, State: state, Tone: tone, ResetsAt: r.ResetsAt}
		if r.Kind == protocol.QuotaKindRequests || r.Kind == protocol.QuotaKindTokens || (r.Kind == protocol.QuotaKindCredits && r.Limit != 100) {
			usedVal := math.Max(0, r.Limit-r.Remaining)
			if r.Exhausted {
				usedVal = r.Limit
			}
			limVal := r.Limit
			bucket.Used = &usedVal
			bucket.Limit = &limVal
		}
		dims = append(dims, dim)
		buckets = append(buckets, bucket)
	}
	if len(dims) == 0 {
		return nil
	}
	chosen := -1
	if display != "" && display != protocol.QuotaDisplayAuto && display != protocol.QuotaDisplayOff {
		for i, b := range buckets {
			if b.ID == display {
				chosen = i
				break
			}
		}
		if chosen == -1 {
			for i, b := range buckets {
				if b.Kind == display {
					chosen = i
					break
				}
			}
		}
	}
	if chosen == -1 {
		chosen = 0
		for i, d := range dims {
			if *d.Remaining < *dims[chosen].Remaining {
				chosen = i
			}
		}
	}
	out := dims[chosen]
	out.Buckets = buckets
	for i, d := range dims {
		if i != chosen {
			out.Details = append(out.Details, d)
		}
	}
	return &out
}

// refreshQuota polls a QuotaReader best-effort on sync, throttled to quotaPollIntervalMs unless force is true.
// On a forced (manual) sync that succeeded without returning fresh readings, any prior exhausted/cooldown state is
// cleared so the user can unblock sends after resetting externally.
func (s *Space) refreshQuota(ctx context.Context, accountID string, adapter provider.Adapter, cred string, force bool) {
	if accountID == "" || cred == "" {
		return
	}
	qr, ok := adapter.(provider.QuotaReader)
	if !ok {
		if force {
			s.clearExhaustedQuota(accountID)
		}
		return
	}
	now := s.now()
	s.mu.Lock()
	last := s.quotaCheckedAt[accountID]
	if !force && last != 0 && now-last < quotaPollIntervalMs {
		s.mu.Unlock()
		return
	}
	s.quotaCheckedAt[accountID] = now
	s.mu.Unlock()
	readings, err := qr.ReadQuota(ctx, cred)
	if err != nil {
		s.observe(accountID, err)
		return
	}
	if len(readings) > 0 {
		s.noteQuota(accountID, readings)
	} else if force {
		s.clearExhaustedQuota(accountID)
	}
}

func (s *Space) clearExhaustedQuota(accountID string) {
	now := s.now()
	s.mu.Lock()
	q, ok := s.quota[accountID]
	cleared := false
	if ok && (q.limitedUntil != 0 || quotaBlocksSend(buildQuota(q, false, protocol.QuotaDisplayAuto, now), now)) {
		delete(s.quota, accountID)
		cleared = true
	}
	s.mu.Unlock()
	if cleared {
		s.notify(nil)
	}
}

func (s *Space) hasActiveQuotaBlock(accountID string) bool {
	now := s.now()
	s.mu.Lock()
	q := s.quota[accountID]
	s.mu.Unlock()
	return quotaBlocksSend(buildQuota(q, false, protocol.QuotaDisplayAuto, now), now)
}

// accountUsage computes local rolling 5h/24h user send activity for an account. Returns nil when zero so idle
// accounts omit the field on the wire.
func (s *Space) accountUsage(ctx context.Context, accountID string, now int64) *protocol.UsageSummary {
	n24, err := s.store.CountUserMessagesSince(ctx, accountID, now-int64(24*time.Hour/time.Millisecond))
	if err != nil || n24 == 0 {
		return nil
	}
	n5, err := s.store.CountUserMessagesSince(ctx, accountID, now-int64(5*time.Hour/time.Millisecond))
	if err != nil {
		n5 = 0
	}
	return &protocol.UsageSummary{Sent5h: n5, Sent24h: n24, Source: protocol.QuotaSourceEstimated}
}

// exhaustedResetAt returns (true, resetsAtMs) when any active quota dimension is currently exhausted (even if the user
// pinned a different non-exhausted bucket for the avatar ring).
func exhaustedResetAt(q *protocol.Quota, now int64) (bool, int64) {
	if q == nil {
		return false, 0
	}
	if len(q.Buckets) > 0 {
		var best int64
		found := false
		for _, b := range q.Buckets {
			if b.State != protocol.QuotaExhausted {
				continue
			}
			if b.ResetsAt > 0 {
				if now >= b.ResetsAt {
					continue
				}
				if !found || b.ResetsAt > best {
					best = b.ResetsAt
				}
				found = true
			} else if !q.Stale {
				found = true
			}
		}
		return found, best
	}
	if q.State != protocol.QuotaExhausted {
		return false, 0
	}
	if q.ResetsAt > 0 {
		return now < q.ResetsAt, q.ResetsAt
	}
	return !q.Stale, 0
}

// quotaBlocksSend reports whether an account's quota is known to be exhausted right now, so a send should stop with
// a clear prompt instead of silently failing against the provider.
func quotaBlocksSend(q *protocol.Quota, now int64) bool {
	blocked, _ := exhaustedResetAt(q, now)
	return blocked
}

// exhaustedSendMessage formats the pre-send refusal message shown when an account's quota is exhausted.
func exhaustedSendMessage(acct store.Account, q *protocol.Quota, now int64) string {
	who := names.Title(names.Default, acct.Provider, acct.DisplayName, acct.NameSource)
	_, resetsAt := exhaustedResetAt(q, now)
	if resetsAt > now {
		return fmt.Sprintf("「%s」的额度已用完（预计 %s 恢复），暂不发送以免失败。如确认已恢复，请在账号页点「立即同步」后再发。",
			who, formatResetWait(resetsAt-now, resetsAt))
	}
	return fmt.Sprintf("「%s」的额度已用完或正在限流，暂不发送以免失败。稍后再试，或在账号页点「立即同步」刷新状态。", who)
}

func formatResetWait(waitMs, resetsAtMs int64) string {
	d := time.Duration(waitMs) * time.Millisecond
	switch {
	case d < time.Minute:
		return "马上"
	case d < time.Hour:
		return fmt.Sprintf("%d 分钟后", int(math.Ceil(d.Minutes())))
	case d < 24*time.Hour:
		h := int(d / time.Hour)
		m := int((d % time.Hour) / time.Minute)
		if m >= 10 {
			return fmt.Sprintf("%d 小时 %d 分钟后", h, m)
		}
		return fmt.Sprintf("%d 小时后", h)
	default:
		return time.UnixMilli(resetsAtMs).UTC().Format("2006-01-02 15:04 UTC")
	}
}
