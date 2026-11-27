package core

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"

	"github.com/nwverse/ensemble/internal/names"
	"github.com/nwverse/ensemble/internal/protocol"
	"github.com/nwverse/ensemble/internal/provider"
	"github.com/nwverse/ensemble/internal/store"
)

// BundleFormat and BundleVersion name the import bundle format (docs/import-bundle.md).
const (
	BundleFormat  = "personal-agent-club/import"
	BundleVersion = 1
)

// Reasons an account of a bundle is skipped or not verified.
const (
	SkipUnsupported = "PROVIDER_UNSUPPORTED"
	SkipNotSelected = "NOT_SELECTED"
	NoCredential    = "NO_CREDENTIAL"
	Removed         = "REMOVED"
	// importNotSent is the error of a send the gateway had queued and never sent: it is not sent by club either.
	importNotSent = "IMPORTED_NOT_SENT"
)

// Bundle is an import bundle: one space's rows as the Lark gateway's agent_* tables hold them (column names as
// keys), the credentials by account id, and the browser identity.
type Bundle struct {
	Format          string                    `json:"format"`
	Version         int                       `json:"version"`
	Accounts        []bundleAccount           `json:"accounts"`
	Sessions        []bundleSession           `json:"sessions"`
	Messages        []bundleMessage           `json:"messages"`
	Outbox          []bundleOutbox            `json:"outbox"`
	Credentials     map[string]string         `json:"credentials"`
	BrowserIdentity *protocol.BrowserIdentity `json:"browserIdentity"`
}

// num is a bigint column: a JSON number, or a string of digits (as node-postgres writes bigint), or null.
type num int64

func (n *num) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(bytes.TrimSpace(b)), `"`)
	if s == "null" || s == "" {
		*n = 0
		return nil
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		f, ferr := strconv.ParseFloat(s, 64)
		if ferr != nil || f != float64(int64(f)) {
			return fmt.Errorf("%s is not an integer", b)
		}
		v = int64(f)
	}
	*n = num(v)
	return nil
}

// flag is a boolean column: true/false, or 0/1.
type flag bool

func (f *flag) UnmarshalJSON(b []byte) error {
	switch strings.Trim(string(bytes.TrimSpace(b)), `"`) {
	case "true", "1", "t":
		*f = true
	case "false", "0", "f", "null", "":
		*f = false
	default:
		return fmt.Errorf("%s is not a boolean", b)
	}
	return nil
}

type bundleAccount struct {
	AccountID         string  `json:"account_id"`
	Provider          string  `json:"provider"`
	DisplayName       string  `json:"display_name"`
	Status            string  `json:"status"`
	ExternalAccountID *string `json:"external_account_id"`
	LastSyncedAt      num     `json:"last_synced_at"`
	LastError         *string `json:"last_error"`
	CreatedAt         num     `json:"created_at"`
	UpdatedAt         num     `json:"updated_at"`
}

type bundleSession struct {
	SessionKey       string          `json:"session_key"`
	AccountID        string          `json:"account_id"`
	Provider         string          `json:"provider"`
	ExternalID       string          `json:"external_id"`
	Title            string          `json:"title"`
	Kind             string          `json:"kind"`
	ParentSessionKey *string         `json:"parent_session_key"`
	CanSend          flag            `json:"can_send"`
	RefJSON          json.RawMessage `json:"ref_json"`
	RankTimeMs       num             `json:"rank_time_ms"`
	OnTopRankTimeMs  num             `json:"on_top_rank_time_ms"`
	UnreadCount      num             `json:"unread_count"`
	ReadTimeMs       num             `json:"read_time_ms"`
	IsHidden         flag            `json:"is_hidden"`
	PreviewText      string          `json:"preview_text"`
	PreviewIsMine    flag            `json:"preview_is_mine"`
	UpdatedAt        num             `json:"updated_at"`
}

type bundleMessage struct {
	SessionKey   string           `json:"session_key"`
	ExternalID   string           `json:"external_id"`
	Position     num              `json:"position"`
	Role         string           `json:"role"`
	BlocksJSON   []protocol.Block `json:"blocks_json"`
	CreateTimeMs num              `json:"create_time_ms"`
}

type bundleOutbox struct {
	ClientID     string  `json:"client_id"`
	SessionKey   string  `json:"session_key"`
	AccountID    string  `json:"account_id"`
	Text         string  `json:"text"`
	Status       string  `json:"status"`
	AttemptCount num     `json:"attempt_count"`
	LastError    *string `json:"last_error"`
	CreatedAt    num     `json:"created_at"`
	UpdatedAt    num     `json:"updated_at"`
}

func str(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// ReadBundle reads an import bundle. The file holds credentials in plain text: one that others than its owner can
// read is refused.
func ReadBundle(path string) (*Bundle, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return nil, badInput("cannot read the bundle: " + err.Error())
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return nil, badInput(fmt.Sprintf("the bundle is readable by others (mode %v); make it 0600 first", fi.Mode().Perm()))
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, badInput("cannot read the bundle: " + err.Error())
	}
	var b Bundle
	if err := json.Unmarshal(raw, &b); err != nil {
		return nil, badInput("the bundle is not valid JSON: " + err.Error())
	}
	if b.Version != BundleVersion || (b.Format != "" && b.Format != BundleFormat) {
		return nil, badInput(fmt.Sprintf("the bundle is not a %s version %d bundle", BundleFormat, BundleVersion))
	}
	return &b, nil
}

// ImportOptions select what an import takes.
type ImportOptions struct {
	// OnlyProviders, when set, imports these providers' accounts only; the others are skipped (NOT_SELECTED).
	OnlyProviders []string
	// DryRun checks the bundle and reports what would be imported, writing nothing and verifying nothing.
	DryRun bool
}

type importPlan struct {
	rows     store.Import
	creds    map[string]string
	result   protocol.ImportResult
	identity *protocol.BrowserIdentity
}

var accountStatuses = []string{protocol.AccountActive, protocol.AccountPaused, protocol.AccountError, store.AccountRemoved, "pending_pairing"}

// planImport checks a bundle against the providers of reg and the accounts already in the space, and turns it into
// rows. Accounts whose provider this build cannot reach are skipped with their sessions, messages, sends and
// credentials, as are those OnlyProviders leaves out. Every imported account gets assistant control off; claimed
// sends become unconfirmed and pending ones failed (IMPORTED_NOT_SENT): an import never leads to a send.
func planImport(b *Bundle, reg *provider.Registry, existing []store.Account, o ImportOptions) (*importPlan, error) {
	p := &importPlan{creds: map[string]string{}, result: protocol.ImportResult{Accounts: []protocol.ImportedAccount{},
		Skipped: []protocol.SkippedAccount{}, DryRun: o.DryRun}, identity: b.BrowserIdentity}
	if p.identity != nil {
		clean, err := cleanIdentity(*p.identity)
		if err != nil {
			return nil, err
		}
		p.identity = &clean
	}
	kept := map[string]store.Account{}
	seen := map[string]bool{}
	for _, a := range b.Accounts {
		switch {
		case !ValidAccountID(a.AccountID):
			return nil, badInput(fmt.Sprintf("account %q: not an account id", a.AccountID))
		case seen[a.AccountID]:
			return nil, badInput(fmt.Sprintf("account %s appears twice", a.AccountID))
		case a.Provider == "":
			return nil, badInput(fmt.Sprintf("account %s: no provider", a.AccountID))
		case !slices.Contains(accountStatuses, a.Status):
			return nil, badInput(fmt.Sprintf("account %s: unknown status %q", a.AccountID, a.Status))
		}
		seen[a.AccountID] = true
		if len(o.OnlyProviders) > 0 && !slices.Contains(o.OnlyProviders, a.Provider) {
			p.result.Skipped = append(p.result.Skipped, protocol.SkippedAccount{AccountID: a.AccountID, Provider: a.Provider,
				DisplayName: a.DisplayName, Reason: SkipNotSelected})
			continue
		}
		if _, ok := reg.Get(a.Provider); !ok {
			p.result.Skipped = append(p.result.Skipped, protocol.SkippedAccount{AccountID: a.AccountID, Provider: a.Provider,
				DisplayName: a.DisplayName, Reason: SkipUnsupported})
			continue
		}
		ext := str(a.ExternalAccountID)
		for _, e := range existing {
			if e.AccountID != a.AccountID && e.Provider == a.Provider && ext != "" && e.ExternalAccountID == ext {
				return nil, protocol.Conflict(fmt.Sprintf("account %s is already paired in this space as %s", a.AccountID, e.AccountID)).
					Because(protocol.ReasonAlreadyPaired)
			}
		}
		status := a.Status
		if status == "pending_pairing" {
			status = protocol.AccountError
		}
		row := store.Account{AccountID: a.AccountID, Provider: a.Provider, DisplayName: cmpOr(a.DisplayName, names.Stored(a.Provider, "")),
			ExternalAccountID: ext, Status: status, LastSyncedAt: int64(a.LastSyncedAt), LastError: str(a.LastError),
			AssistantControl: string(ControlOff), CreatedAt: int64(a.CreatedAt), UpdatedAt: int64(a.UpdatedAt)}
		kept[a.AccountID] = row
		p.rows.Accounts = append(p.rows.Accounts, row)
	}
	for id := range b.Credentials {
		if !seen[id] {
			return nil, badInput(fmt.Sprintf("a credential for account %s, which is not in the bundle", id))
		}
	}
	sessions := map[string]store.Session{}
	for _, x := range b.Sessions {
		prov, acct, ext, ok := ParseSessionKey(x.SessionKey)
		if !ok || prov != x.Provider || acct != x.AccountID || ext != x.ExternalID {
			return nil, badInput(fmt.Sprintf("session %q: the key does not match its provider, account and external id", x.SessionKey))
		}
		if !seen[x.AccountID] {
			return nil, badInput(fmt.Sprintf("session %s: its account is not in the bundle", x.SessionKey))
		}
		if _, ok := kept[x.AccountID]; !ok {
			continue
		}
		if !slices.Contains([]string{protocol.KindMain, protocol.KindSide, protocol.KindTask}, x.Kind) {
			return nil, badInput(fmt.Sprintf("session %s: unknown kind %q", x.SessionKey, x.Kind))
		}
		ref := map[string]string{}
		if len(x.RefJSON) > 0 && string(x.RefJSON) != "null" {
			var all map[string]any
			if err := json.Unmarshal(x.RefJSON, &all); err != nil {
				return nil, badInput(fmt.Sprintf("session %s: ref_json is not an object", x.SessionKey))
			}
			for k, v := range all {
				if s, ok := v.(string); ok {
					ref[k] = s
				}
			}
		}
		avatar := ref["avatarUrl"]
		delete(ref, "avatarUrl")
		row := store.Session{SessionKey: x.SessionKey, AccountID: x.AccountID, Provider: x.Provider, ExternalID: x.ExternalID,
			Title: x.Title, Kind: x.Kind, ParentSessionKey: str(x.ParentSessionKey), CanSend: bool(x.CanSend), AvatarURL: avatar,
			Ref: ref, RankTimeMs: int64(x.RankTimeMs), OnTopRankTimeMs: int64(x.OnTopRankTimeMs), ReadTimeMs: int64(x.ReadTimeMs),
			UnreadCount: int(x.UnreadCount), IsHidden: bool(x.IsHidden), PreviewText: x.PreviewText,
			PreviewIsMine: bool(x.PreviewIsMine), UpdatedAt: int64(x.UpdatedAt)}
		sessions[x.SessionKey] = row
		p.rows.Sessions = append(p.rows.Sessions, row)
	}
	inBundle := func(key string) (store.Session, bool, bool) {
		_, acct, _, ok := ParseSessionKey(key)
		if !ok || !seen[acct] {
			return store.Session{}, false, false
		}
		s, ok := sessions[key]
		return s, ok, true
	}
	counts := map[string]*protocol.ImportedAccount{}
	for _, a := range p.rows.Accounts {
		counts[a.AccountID] = &protocol.ImportedAccount{AccountID: a.AccountID, Provider: a.Provider, DisplayName: a.DisplayName}
	}
	for _, x := range p.rows.Sessions {
		counts[x.AccountID].Sessions++
	}
	for _, m := range b.Messages {
		s, ok, known := inBundle(m.SessionKey)
		if !known {
			return nil, badInput(fmt.Sprintf("message %s: its session %s is not in the bundle", m.ExternalID, m.SessionKey))
		}
		if !ok {
			continue
		}
		if m.ExternalID == "" || m.Role == "" {
			return nil, badInput(fmt.Sprintf("a message of %s has no external_id or role", m.SessionKey))
		}
		blocks := m.BlocksJSON
		if blocks == nil {
			blocks = []protocol.Block{}
		}
		p.rows.Messages = append(p.rows.Messages, store.Message{SessionKey: m.SessionKey, ExternalID: m.ExternalID,
			Role: m.Role, Blocks: blocks, CreateTimeMs: int64(m.CreateTimeMs), Position: int64(m.Position)})
		c := counts[s.AccountID]
		c.Messages++
		c.LatestMessageMs = max(c.LatestMessageMs, int64(m.CreateTimeMs))
	}
	for _, o := range b.Outbox {
		s, ok, known := inBundle(o.SessionKey)
		if !known || o.ClientID == "" {
			return nil, badInput(fmt.Sprintf("send %q: its session %s is not in the bundle", o.ClientID, o.SessionKey))
		}
		if !ok {
			continue
		}
		if o.AccountID != s.AccountID {
			return nil, badInput(fmt.Sprintf("send %s: its account is not its session's", o.ClientID))
		}
		row := store.Outbox{ClientID: o.ClientID, SessionKey: o.SessionKey, AccountID: o.AccountID, Text: o.Text,
			Status: o.Status, LastError: str(o.LastError), AttemptCount: int(o.AttemptCount), CreatedAt: int64(o.CreatedAt),
			UpdatedAt: int64(o.UpdatedAt)}
		switch o.Status {
		case protocol.LocalClaimed:
			row.Status, row.LastError = protocol.LocalUnconfirmed, "SEND_UNCONFIRMED"
		case protocol.LocalPending:
			row.Status, row.LastError = protocol.LocalFailed, importNotSent
		case protocol.LocalSent, protocol.LocalUnconfirmed, protocol.LocalFailed:
		default:
			return nil, badInput(fmt.Sprintf("send %s: unknown status %q", o.ClientID, o.Status))
		}
		p.rows.Outbox = append(p.rows.Outbox, row)
	}
	for id, c := range b.Credentials {
		if _, ok := kept[id]; ok {
			if err := checkCredential(strings.TrimSpace(c)); err != nil {
				return nil, badInput(fmt.Sprintf("the credential of %s must be 8 to 8192 characters", id))
			}
			p.creds[id] = strings.TrimSpace(c)
		}
	}
	for _, a := range p.rows.Accounts {
		c := counts[a.AccountID]
		if _, ok := p.creds[a.AccountID]; ok {
			c.HasCredential = true
		}
		p.result.Accounts = append(p.result.Accounts, *c)
	}
	return p, nil
}

// PlanImport is a dry run of an import into a space holding existing: it reads and checks the bundle and answers
// what would be imported. It writes nothing and calls no provider.
func PlanImport(path string, reg *provider.Registry, existing []store.Account, o ImportOptions) (protocol.ImportResult, error) {
	b, err := ReadBundle(path)
	if err != nil {
		return protocol.ImportResult{}, err
	}
	o.DryRun = true
	p, err := planImport(b, reg, existing, o)
	if err != nil {
		return protocol.ImportResult{}, err
	}
	return p.result, nil
}

// Import is club.import: it writes a bundle's rows into the space as they are (idempotent: rows are keyed by
// account id, session key, message external id and client id), stores the credentials, and then verifies each
// imported account read-only. It sends nothing and does not sync; the bundle file is left in place.
func (s *Space) Import(ctx context.Context, path string, o ImportOptions) (protocol.ImportResult, error) {
	if path == "" {
		return protocol.ImportResult{}, badInput("path is required")
	}
	b, err := ReadBundle(path)
	if err != nil {
		return protocol.ImportResult{}, err
	}
	existing, err := s.store.ListAccounts(ctx)
	if err != nil {
		return protocol.ImportResult{}, err
	}
	p, err := planImport(b, s.club.opts.Registry, existing, o)
	if err != nil || o.DryRun {
		return p.resultOrZero(), err
	}
	for id, c := range p.creds {
		if err := s.vault.Put(ctx, id, c); err != nil {
			return protocol.ImportResult{}, err
		}
	}
	if err := s.store.Import(ctx, p.rows); err != nil {
		return protocol.ImportResult{}, err
	}
	if p.identity != nil {
		if err := s.putIdentity(ctx, *p.identity); err != nil {
			return protocol.ImportResult{}, err
		}
	}
	s.notify(nil)
	for i, r := range p.result.Accounts {
		r.Verified, r.Error = s.verifyImported(ctx, p.rows.Accounts[i])
		c, err := s.store.Counts(ctx, r.AccountID)
		if err != nil {
			return protocol.ImportResult{}, err
		}
		r.Sessions, r.Messages, r.LatestMessageMs = c.Sessions, c.Messages, c.LatestMessageMs
		p.result.Accounts[i] = r
	}
	return p.result, nil
}

func (p *importPlan) resultOrZero() protocol.ImportResult {
	if p == nil {
		return protocol.ImportResult{}
	}
	return p.result
}

func (s *Space) verifyImported(ctx context.Context, a store.Account) (bool, string) {
	if a.Status == store.AccountRemoved {
		return false, Removed
	}
	adapter, ok := s.club.opts.Registry.Get(a.Provider)
	if !ok {
		return false, SkipUnsupported
	}
	cred, found, err := s.vault.Get(ctx, a.AccountID)
	if err != nil || !found {
		return false, NoCredential
	}
	remote, err := adapter.Verify(ctx, cred)
	s.observe(a.AccountID, err)
	if err != nil {
		return false, provider.CodeOf(err)
	}
	s.noteQuota(a.AccountID, remote.Quota)
	if a.ExternalAccountID != "" && remote.ExternalAccountID != a.ExternalAccountID {
		return false, protocol.ReasonAccountMismatch
	}
	return true, ""
}
