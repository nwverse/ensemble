package extension

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/nwverse/persona/sdk/go/wire"

	"github.com/nwverse/personal-agent-club/internal/core"
	"github.com/nwverse/personal-agent-club/internal/protocol"
	"github.com/nwverse/personal-agent-club/internal/provider/echo"
)

const (
	providerMuse = "muse"

	methodMachinesPairLink = "machines.pair_link"
	methodMachinesStatus   = "machines.status"
)

type machinePairLinkParams struct {
	MachineID string `json:"machine_id,omitempty"`
	Name      string `json:"name,omitempty"`
	OS        string `json:"os,omitempty"`
	Icon      string `json:"icon,omitempty"`
}

type machinePairLinkResult struct {
	MachineID    string    `json:"machine_id"`
	Name         string    `json:"name"`
	Link         string    `json:"link"`
	ExpiresAt    time.Time `json:"expires_at"`
	Pending      bool      `json:"pending"`
	InstallLinux string    `json:"install_linux,omitempty"`
}

type machineStatusParams struct {
	MachineID string `json:"machine_id"`
}

type machineStatusResult struct {
	MachineID  string     `json:"machine_id"`
	Name       string     `json:"name,omitempty"`
	Exists     bool       `json:"exists"`
	Online     bool       `json:"online"`
	NodeOnline bool       `json:"node_online"`
	Pending    bool       `json:"pending"`
	LastSeen   *time.Time `json:"last_seen,omitempty"`
	Error      string     `json:"error,omitempty"`
}

func takeoverSupported(provider string) bool {
	switch provider {
	case grokBot, providerMuse, echo.Name:
		return true
	default:
		return false
	}
}

// takeoverMachineID returns a stable, hub-valid machine id for an account.
func takeoverMachineID(acct protocol.Account) string {
	p := strings.NewReplacer("_", "-", ".", "-").Replace(strings.ToLower(acct.Provider))
	id := "pac-" + p + "-" + strings.ToLower(acct.AccountID)
	if len(id) > 40 {
		id = id[:40]
	}
	return strings.TrimRight(id, "-")
}

func takeoverSessionKey(ctx context.Context, space *core.Space, accountID string) (string, error) {
	list, err := space.Sessions(ctx)
	if err != nil {
		return "", err
	}
	var fallback string
	for _, s := range list.Sessions {
		if s.AccountID != accountID || !s.CanSend {
			continue
		}
		if s.Kind == protocol.KindMain {
			return s.SessionKey, nil
		}
		if fallback == "" {
			fallback = s.SessionKey
		}
	}
	if fallback != "" {
		return fallback, nil
	}
	return "", protocol.NotFound("no sendable session for this account yet; sync or start a chat first")
}

func takeoverPrompt(provider string, res machinePairLinkResult) string {
	installLine := res.InstallLinux
	if installLine == "" {
		installLine = fmt.Sprintf("~/.persona/bin/persona-link pair '%s'", res.Link)
	}
	switch provider {
	case providerMuse:
		return fmt.Sprintf(`Connect this VM to my Persona hub as a paired machine (%s) using Persona Link active mode.
Rules: never print tokens, pairing links, or proxy credentials in chat.

1. Keep everything inside the persistent home directory ($HOME/.persona/bin, $HOME/.persona/link.json, $HOME/.persona/ensure-persona.sh) because everything outside $HOME is wiped when the VM recycles.
2. Install or update persona-node and persona-link (linux-amd64) and pair with:
   %s
   (Keep the current environment's HTTPS_PROXY / HTTP_PROXY active so persona-link connects through the egress proxy.)
3. Create an idempotent supervisor script $HOME/.persona/ensure-persona.sh that uses flock + setsid nohup to ensure both:
   - $HOME/.persona/bin/persona-node node --attach >> $HOME/.persona/node-attach.log 2>&1
   - $HOME/.persona/bin/persona-link run --log $HOME/.persona/link.log >/dev/null 2>&1
   are running (inheriting the current shell's HTTPS_PROXY).
4. Run $HOME/.persona/ensure-persona.sh now, add a guard to $HOME/.bashrc, and include it in your periodic Heartbeat task so the node auto-recovers after VM recycles.
5. Report back when persona-link status shows connected.`, res.MachineID, installLine)
	case grokBot:
		return fmt.Sprintf(`Connect this Grok Bot box to my Persona hub as a paired machine (%s) using Persona Link active mode.
Rules: never print tokens or pairing links in chat.

1. Install persona-node and persona-link (linux-amd64) into $HOME/.persona/bin/ and pair with:
   %s
2. Supervise both persona-node (node --attach) and persona-link (run --log $HOME/.persona/link.log) via flock + setsid nohup scripts in $HOME/.persona/ (never inside an interactive terminal window).
3. Add an xfce autostart .desktop entry and a $HOME/.bashrc guard so both survive reboots.
4. Report back once persona-link status shows connected.`, res.MachineID, installLine)
	default:
		return fmt.Sprintf("Pair this environment as Persona machine %s using: %s", res.MachineID, installLine)
	}
}

func wakePrompt(provider, machineID string) string {
	switch provider {
	case providerMuse:
		return fmt.Sprintf("My Persona machine (%s) appears offline (likely after a VM recycle). Please run $HOME/.persona/ensure-persona.sh with the current HTTPS_PROXY environment so persona-node and persona-link reconnect, and confirm once persona-link status is connected (do not print any credentials).", machineID)
	case grokBot:
		return fmt.Sprintf("My Persona machine (%s) appears offline. Please check pgrep -af 'persona-node|persona-link' and restart the $HOME/.persona/ supervisors via setsid nohup, then confirm once connected (do not print any credentials).", machineID)
	default:
		return fmt.Sprintf("Wake Persona machine %s and confirm once persona-link is connected.", machineID)
	}
}

func (a *accounts) machineSection(ctx context.Context, l lang, acct protocol.Account) (wire.Section, bool) {
	hub := a.hub.Load()
	mid := takeoverMachineID(acct)
	var st machineStatusResult
	var statusErr error
	if hub != nil {
		statusErr = hub.Call(ctx, methodMachinesStatus, machineStatusParams{MachineID: mid}, &st)
	}
	sec := wire.Section{
		Title: l.t("配对为机器（夺舍）", "Pair as Machine"),
		Footer: l.t("向该云端 Agent 发送引导指令，让它的沙箱通过 Persona Link 接入你的 Hub。新机器仍需在「机器」页确认「这是我的电脑」。",
			"Sends bootstrap instructions to this cloud agent so its VM connects to your hub via Persona Link. New computers still wait for confirmation on the Machines page."),
	}
	if hub == nil {
		sec.Items = []wire.Component{{Type: "row", Title: l.t("状态", "Status"), Value: l.t("未连接到 Hub", "Hub unavailable")}}
		return sec, false
	}
	if statusErr != nil {
		sec.Items = []wire.Component{{Type: "row", Title: l.t("状态", "Status"), Value: statusErr.Error(),
			Badge: &wire.Badge{Text: l.t("需权限", "Permission needed"), Tone: wire.ToneCaution}}}
		return sec, false
	}
	switch {
	case !st.Exists:
		sec.Items = []wire.Component{
			{Type: "row", Title: l.t("状态", "Status"), Value: l.t("尚未配对为机器", "Not paired yet"),
				Badge: &wire.Badge{Text: l.t("未配对", "Unpaired"), Tone: wire.ToneNeutral}},
			{Type: "button", Title: l.t("夺舍为机器", "Pair as Machine"), Action: &wire.Action{
				ID:      actTakeover,
				Confirm: l.t("将生成一次性配对链接并发送给该 Agent 的会话，引导它安装并连接 Persona Link。是否继续？", "This generates a one-time pairing link and sends bootstrap instructions to this agent's chat. Continue?"),
			}},
		}
		return sec, false
	case st.Pending:
		badge := &wire.Badge{Text: l.t("待确认", "Pending"), Tone: wire.ToneCaution}
		val := l.t("等待连接… 请在「机器」页确认「这是我的电脑」", "Waiting for connection… confirm on the Machines page")
		if st.Online {
			val = l.t("已连上 Hub，请前往「机器」页点「这是我的电脑」", "Connected to hub; confirm \"This is my computer\" on Machines")
		}
		sec.Items = []wire.Component{
			{Type: "row", Title: l.t("机器 ID", "Machine ID"), Value: st.MachineID, Badge: badge},
			{Type: "row", Title: l.t("状态", "Status"), Value: val},
			{Type: "button", Title: l.t("重新发送夺舍指令", "Resend bootstrap"), Action: &wire.Action{
				ID:      actTakeover,
				Confirm: l.t("重新生成一次性配对链接并发送引导指令？", "Generate a fresh pairing link and resend bootstrap instructions?"),
			}},
		}
		return sec, true
	case st.Online:
		badge := &wire.Badge{Text: l.t("在线", "Online"), Tone: wire.TonePositive}
		if !st.NodeOnline {
			badge = &wire.Badge{Text: l.t("节点离线", "Node offline"), Tone: wire.ToneCaution}
		}
		sec.Items = []wire.Component{
			{Type: "row", Title: l.t("机器 ID", "Machine ID"), Value: st.MachineID, Badge: badge},
			{Type: "button", Title: l.t("发送唤醒指令", "Wake Machine"), Action: &wire.Action{ID: actWake}},
		}
		return sec, false
	default:
		val := l.t("已离线（云端 VM 可能已回收重建）", "Offline (cloud VM may have recycled)")
		if st.Error != "" {
			val = st.Error
		}
		sec.Items = []wire.Component{
			{Type: "row", Title: l.t("机器 ID", "Machine ID"), Value: st.MachineID,
				Badge: &wire.Badge{Text: l.t("离线", "Offline"), Tone: wire.ToneDanger}},
			{Type: "row", Title: l.t("状态", "Status"), Value: val},
			{Type: "button", Title: l.t("唤醒机器", "Wake Machine"), Action: &wire.Action{ID: actWake}},
		}
		return sec, false
	}
}

func (a *accounts) runTakeover(ctx context.Context, acctID string) (string, error) {
	hub := a.hub.Load()
	if hub == nil {
		return "", fmt.Errorf("hub unavailable")
	}
	space, err := a.club.Space(ctx, core.DefaultSpace)
	if err != nil {
		return "", err
	}
	acct, err := space.Account(ctx, acctID)
	if err != nil {
		return "", err
	}
	if !takeoverSupported(acct.Provider) {
		return "", protocol.Unsupported("provider does not support machine takeover")
	}
	sessionKey, err := takeoverSessionKey(ctx, space, acct.AccountID)
	if err != nil {
		return "", err
	}
	mid := takeoverMachineID(acct)
	var pairRes machinePairLinkResult
	err = hub.Call(ctx, methodMachinesPairLink, machinePairLinkParams{
		MachineID: mid,
		Name:      accountName(acct),
		OS:        "linux",
		Icon:      "server",
	}, &pairRes)
	if err != nil {
		return "", err
	}
	text := takeoverPrompt(acct.Provider, pairRes)
	if _, err := space.Send(ctx, protocol.SendParams{
		SessionKey: sessionKey,
		ClientID:   uuid.NewString(),
		Text:       text,
	}); err != nil {
		return "", err
	}
	return pairRes.MachineID, nil
}

func (a *accounts) runWake(ctx context.Context, acctID string) (string, error) {
	space, err := a.club.Space(ctx, core.DefaultSpace)
	if err != nil {
		return "", err
	}
	acct, err := space.Account(ctx, acctID)
	if err != nil {
		return "", err
	}
	sessionKey, err := takeoverSessionKey(ctx, space, acct.AccountID)
	if err != nil {
		return "", err
	}
	mid := takeoverMachineID(acct)
	if _, err := space.Send(ctx, protocol.SendParams{
		SessionKey: sessionKey,
		ClientID:   uuid.NewString(),
		Text:       wakePrompt(acct.Provider, mid),
	}); err != nil {
		return "", err
	}
	return mid, nil
}
