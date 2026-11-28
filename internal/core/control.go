package core

import (
	"fmt"

	"github.com/nwverse/ensemble/internal/store"
)

// Control is an account's assistant control level (assistant_control): how far the Persona assistant may drive the
// agents of that account through club's own tools.
type Control string

const (
	// ControlOff: the assistant does not see the account. The default for new and imported accounts.
	ControlOff Control = "off"
	// ControlRead: list and read.
	ControlRead Control = "read"
	// ControlConfirm: list and read; every send, start and interrupt is confirmed by the user first.
	ControlConfirm Control = "confirm"
	// ControlFollow: list and read; sends and interrupts on a session holding a valid, unexpired, unexhausted
	// FollowLease go without asking each time (and in the background when allow_background is set on the lease,
	// still bounded by max_turns and expires_at); writes without an active matching lease degrade to confirm
	// (or deny when unattended).
	ControlFollow Control = "follow"
	// ControlAuto: list and read; sends, starts and interrupts go without asking each time.
	ControlAuto Control = "auto"
)

// DefaultControl is the level of an account nobody has set.
const DefaultControl = ControlOff

// ParseControl reads a level; "" is the default.
func ParseControl(s string) (Control, error) {
	switch c := Control(s); c {
	case "":
		return DefaultControl, nil
	case ControlOff, ControlRead, ControlConfirm, ControlFollow, ControlAuto:
		return c, nil
	}
	return "", fmt.Errorf("unknown assistant_control %q", s)
}

// RevokesLeasesOnControlChange reports whether switching an account's control level to `to` must automatically revoke
// all active follow leases on that account. Downgrading to off, read, or confirm revokes all active leases so no
// session continues bypassing per-turn confirmation after the owner steps the account down.
func RevokesLeasesOnControlChange(to Control) bool {
	switch to {
	case ControlOff, ControlRead, ControlConfirm:
		return true
	default:
		return false
	}
}

// Operation is something the assistant asks to do with an agent.
type Operation string

const (
	OpList      Operation = "list"
	OpRead      Operation = "read"
	OpSend      Operation = "send"
	OpStart     Operation = "start"
	OpInterrupt Operation = "interrupt"
	// OpManage is pairing, unpairing, replacing a credential, signing in, issuing/revoking follow leases, repository
	// permissions or changing assistant_control itself: never the assistant's, at any level.
	OpManage Operation = "manage"
)

// Writes says whether op changes anything at the agent.
func (op Operation) Writes() bool {
	return op == OpSend || op == OpStart || op == OpInterrupt || op == OpManage
}

// Verdict is what Decide allows.
type Verdict string

const (
	Allow   Verdict = "allow"
	Confirm Verdict = "confirm" // only after the user confirms this one call
	Deny    Verdict = "deny"
)

// Decide is whether the assistant may do op on an account at level without an active session follow lease. unattended
// is a call with nobody present to confirm (a scheduled task, a background follow-up): without an active
// background-enabled follow lease it may only read, at any level. Unknown levels and operations (including OpManage)
// are always denied.
func Decide(level Control, op Operation, unattended bool) Verdict {
	switch op {
	case OpList, OpRead, OpSend, OpStart, OpInterrupt:
	default:
		return Deny
	}
	if op.Writes() && unattended {
		return Deny
	}
	switch level {
	case ControlRead:
		if op.Writes() {
			return Deny
		}
		return Allow
	case ControlConfirm, ControlFollow:
		if op.Writes() {
			return Confirm
		}
		return Allow
	case ControlAuto:
		return Allow
	}
	return Deny
}

// DecideWithLease evaluates whether the assistant may perform op on sessionKey of an account at level, taking an
// optional FollowLease into account at timestamp now (Unix ms).
//
// Red lines:
//   - OpManage (and any unknown operation) is ALWAYS denied at every level, even with a lease.
//   - Under ControlFollow, only OpSend and OpInterrupt on the exact sessionKey of an active, unexpired (ExpiresAt > now),
//     unexhausted (UsedTurns < MaxTurns), unrevoked (RevokedAt == 0) lease are allowed directly.
//   - When unattended is true under ControlFollow, the active matching lease must also have AllowBackground == true
//     (still strictly capped by the same MaxTurns and ExpiresAt); otherwise it is denied.
//   - When no matching active lease applies under ControlFollow, attended writes degrade to Confirm and unattended
//     writes are denied.
func DecideWithLease(level Control, op Operation, unattended bool, sessionKey string, lease *store.FollowLease, now int64) Verdict {
	switch op {
	case OpList, OpRead, OpSend, OpStart, OpInterrupt:
	default:
		return Deny
	}
	if !op.Writes() {
		return Decide(level, op, unattended)
	}
	if level == ControlFollow {
		if (op == OpSend || op == OpInterrupt) && sessionKey != "" && lease != nil &&
			lease.SessionKey == sessionKey && lease.IsActive(now) {
			if unattended && !lease.AllowBackground {
				return Deny
			}
			return Allow
		}
		if unattended {
			return Deny
		}
		return Confirm
	}
	return Decide(level, op, unattended)
}
