// Ensemble in Orca's sidebar: your personal agents, grouped by account, a conversation and a
// reply box. Every hub call goes through the worker (src/worker.ts); this panel has no network.
import {
  atom,
  Badge,
  Box,
  Button,
  Callout,
  Card,
  EmptyState,
  Heading,
  IconButton,
  Input,
  List,
  ListItem,
  Markdown,
  plugin,
  read,
  RelativeTime,
  Row,
  set,
  Spinner,
  Stack,
  Text,
  Textarea,
  type PanelContext
} from 'orca'
import type { HubErrorCode, Message, Overview, Session } from './protocol'

type Problem = { code: HubErrorCode; message: string }

const config = atom<{ hubUrl: string; hasToken: boolean } | null>(null)
const editing = atom('editing', false)
const urlDraft = atom('urlDraft', '')
const tokenDraft = atom('tokenDraft', '')
const saving = atom(false)
const configProblem = atom<Problem | null>(null)

const overview = atom<Overview | null>(null)
const overviewProblem = atom<Problem | null>(null)
const refreshing = atom(false)

const selected = atom<string | null>('selected', null, { persist: 'plugin' })
const thread = atom<{ id: string; messages: Message[]; rank: number } | null>(null)
const threadProblem = atom<Problem | null>(null)
const draft = atom('draft', '')
/** The UUID for the text in `draft`, kept so a retry after a timeout is the same message. */
const clientId = atom<string | null>('clientId', null)
const sending = atom(false)
const sendProblem = atom<Problem | null>(null)

const SHOWN_PER_ACCOUNT = 12

function uuid(): string {
  const c = (globalThis as any).crypto
  if (c?.randomUUID) return c.randomUUID()
  const b = Array.from({ length: 16 }, () => Math.floor(Math.random() * 256))
  b[6] = (b[6] & 0x0f) | 0x40
  b[8] = (b[8] & 0x3f) | 0x80
  const h = b.map((x) => x.toString(16).padStart(2, '0')).join('')
  return `${h.slice(0, 8)}-${h.slice(8, 12)}-${h.slice(12, 16)}-${h.slice(16, 20)}-${h.slice(20)}`
}

async function loadConfig(): Promise<void> {
  const c = await plugin.request('config.get')
  set(config, c)
  if (!read(urlDraft)) set(urlDraft, c.hubUrl)
}

async function refresh(): Promise<void> {
  set(refreshing, true)
  try {
    const res = await plugin.request('overview.get', { refresh: true })
    if (res.ok) {
      set(overview, res.value)
      set(overviewProblem, null)
    } else set(overviewProblem, { code: res.code, message: res.message })
  } finally {
    set(refreshing, false)
  }
}

async function save(): Promise<void> {
  set(saving, true)
  set(configProblem, null)
  try {
    const res = await plugin.request('config.save', { hubUrl: read(urlDraft), token: read(tokenDraft) || undefined }, { timeoutMs: 40_000 })
    if (!res.ok) {
      set(configProblem, { code: res.code, message: res.message })
      return
    }
    set(tokenDraft, '')
    set(editing, false)
    await loadConfig()
    await refresh()
  } finally {
    set(saving, false)
  }
}

async function disconnect(): Promise<void> {
  await plugin.request('config.clear')
  set(overview, null)
  set(selected, null)
  set(thread, null)
  await loadConfig()
}

async function loadThread(id: string, rank: number): Promise<void> {
  const res = await plugin.request('messages.get', { id })
  if (read(selected) !== id) return
  if (res.ok) {
    set(thread, { id, messages: res.value.messages, rank })
    set(threadProblem, null)
  } else set(threadProblem, { code: res.code, message: res.message })
}

async function open(s: Session): Promise<void> {
  set(selected, s.id)
  set(thread, null)
  set(threadProblem, null)
  set(sendProblem, null)
  await loadThread(s.id, s.rankTimeMs)
  if (s.unreadCount > 0) {
    await plugin.request('session.read', { id: s.id })
    const ov = read(overview)
    if (ov) set(overview, { ...ov, sessions: ov.sessions.map((x) => (x.id === s.id ? { ...x, unreadCount: 0 } : x)) })
  }
}

async function send(s: Session): Promise<void> {
  const text = read(draft)
  if (!text.trim() || read(sending)) return
  let id = read(clientId)
  if (!id) {
    id = uuid()
    set(clientId, id)
  }
  set(sending, true)
  set(sendProblem, null)
  try {
    const res = await plugin.request('message.send', { id: s.id, text, clientId: id }, { timeoutMs: 60_000 })
    if (!res.ok) {
      set(sendProblem, { code: res.code, message: res.message })
      return
    }
    set(draft, '')
    set(clientId, null)
    await loadThread(s.id, s.rankTimeMs)
  } catch (err) {
    // A timeout leaves `clientId` as it is: pressing Send again repeats this message, never a new one.
    set(sendProblem, { code: 'unreachable', message: `没有收到 hub 的确认：${(err as Error)?.message ?? err}。再按一次发送会重试同一条消息。` })
  } finally {
    set(sending, false)
  }
}

export function setup(ctx: PanelContext): void {
  ctx.subscriptions.push(
    plugin.on('overview.changed', (ov) => {
      set(overview, ov)
      set(overviewProblem, null)
      const id = read(selected)
      const s = id ? ov.sessions.find((x) => x.id === id) : undefined
      const t = read(thread)
      if (s && t && t.id === s.id && s.rankTimeMs !== t.rank) void loadThread(s.id, s.rankTimeMs)
    })
  )
  void loadConfig().then(() => {
    if (read(config)?.hasToken && read(config)?.hubUrl) {
      void refresh().then(() => {
        const id = read(selected)
        const s = id ? read(overview)?.sessions.find((x) => x.id === id) : undefined
        if (s) void open(s)
        else if (id) set(selected, null)
      })
    }
  })
}

function problemCallout(p: Problem | null, title: string) {
  if (!p) return null
  const hint =
    p.code === 'unauthorized' || p.code === 'forbidden'
      ? '在 Persona 设置 › 设备 里重新建一个设备凭据（勾选 agents.read 和 agents.control），再在这里保存。'
      : p.code === 'unreachable'
        ? '看看 hub 地址对不对、这台电脑能不能访问它（比如 Tailscale 是否在线）。'
        : ''
  return (
    <Callout tone="danger" title={title} icon="warning">
      <Text size="sm">{p.message}</Text>
      {hint ? <Text size="sm" muted>{hint}</Text> : null}
    </Callout>
  )
}

function Setup() {
  const c = read(config)
  return (
    <Stack gap="md">
      <Heading level={2}>连接 Persona</Heading>
      <Text size="sm" muted>
        Ensemble 通过你的 Persona hub 读取和回复个人 Agent。在 Persona 的 设置 › 设备 新建一个设备凭据，权限勾选 agents.read 和
        agents.control，把它粘贴到下面。凭据存在 Orca 的加密存储里。
      </Text>
      <Input label="Hub 地址" placeholder="https://persona.example.ts.net" value={read(urlDraft)} onChange={(v) => set(urlDraft, v)} />
      <Input
        label="设备凭据"
        type="password"
        placeholder={c?.hasToken ? '已保存；留空则继续用它' : '粘贴设备凭据'}
        value={read(tokenDraft)}
        onChange={(v) => set(tokenDraft, v)}
        onSubmit={() => void save()}
      />
      {problemCallout(read(configProblem), '没能连上')}
      <Row gap="xs">
        <Button tone="accent" icon="link" disabled={read(saving)} onPress={save}>
          {read(saving) ? '正在检查…' : '保存并连接'}
        </Button>
        {c?.hasToken && c.hubUrl ? (
          <Button variant="ghost" onPress={() => set(editing, false)}>
            取消
          </Button>
        ) : null}
        {c?.hasToken ? (
          <Button variant="ghost" tone="danger" onPress={disconnect}>
            断开
          </Button>
        ) : null}
      </Row>
    </Stack>
  )
}

function AgentList() {
  const ov = read(overview)
  const problem = read(overviewProblem)
  if (!ov) return problem ? problemCallout(problem, '读不到 Agent') : <Spinner label="正在读取 Agent…" />
  const byAccount = new Map<string, Session[]>()
  for (const s of ov.sessions) byAccount.set(s.accountId, [...(byAccount.get(s.accountId) ?? []), s])
  const accounts = ov.accounts.filter((a) => byAccount.has(a.accountId) || a.lastError)
  if (accounts.length === 0) {
    return (
      <EmptyState title="还没有 Agent" icon="bot" description="在 Persona 的 设置 › 扩展 › Ensemble › 账号 里配对 Muse、Gemini Tasks 等账号。" />
    )
  }
  return (
    <Stack gap="md">
      {problemCallout(problem, '刷新失败，下面是上次的结果')}
      {accounts.map((a) => {
        const list = byAccount.get(a.accountId) ?? []
        const unread = list.reduce((n, s) => n + s.unreadCount, 0)
        return (
          <Stack gap="xs" key={a.accountId}>
            <Row justify="between">
              <Text weight="semibold" truncate>
                {a.displayName}
              </Text>
              <Row gap="xs">
                {unread ? <Badge tone="accent">{String(unread)}</Badge> : null}
                <Badge>{a.provider}</Badge>
              </Row>
            </Row>
            {a.lastError ? <Text size="xs" tone="danger">{a.lastError}</Text> : null}
            {list.length ? (
              <List label={a.displayName}>
                {list.slice(0, SHOWN_PER_ACCOUNT).map((s) => (
                  <ListItem key={s.id} title={s.title} icon={s.unreadCount ? 'bell' : 'agent'} onPress={() => void open(s)}>
                    <Row gap="xs">
                      {s.unreadCount ? <Badge tone="accent">{`${s.unreadCount} 未读`}</Badge> : null}
                      {s.rankTimeMs ? <RelativeTime at={s.rankTimeMs} muted /> : null}
                    </Row>
                  </ListItem>
                ))}
              </List>
            ) : null}
            {list.length > SHOWN_PER_ACCOUNT ? (
              <Text size="xs" muted>{`还有 ${list.length - SHOWN_PER_ACCOUNT} 个更早的会话，在 Persona 里查看`}</Text>
            ) : null}
          </Stack>
        )
      })}
    </Stack>
  )
}

const STATUS_LABEL: Record<string, string> = { pending: '排队中', claimed: '发送中', unconfirmed: '未确认', failed: '发送失败' }

function Thread({ s }: { s: Session }) {
  const t = read(thread)
  const problem = read(threadProblem)
  const ov = read(overview)
  const who = ov?.accounts.find((a) => a.accountId === s.accountId)?.displayName ?? s.provider
  return (
    <Stack gap="md">
      <Row gap="xs">
        <Button variant="ghost" size="sm" tooltip="返回列表" onPress={() => set(selected, null)}>
          ‹ 返回
        </Button>
        <Stack gap="none">
          <Text weight="semibold" truncate>
            {s.title}
          </Text>
          <Text size="xs" muted truncate>
            {who}
          </Text>
        </Stack>
      </Row>
      {problemCallout(problem, '读不到这个会话')}
      {!t && !problem ? <Spinner label="正在读取消息…" /> : null}
      {t && t.messages.length === 0 ? <EmptyState title="还没有消息" icon="file-text" /> : null}
      {t
        ? t.messages.map((m) => {
            const mine = m.role === 'user'
            return (
              <Box key={m.id} padding="sm" radius="md" border={!mine} background={mine ? 'muted' : 'none'} direction="column" gap="xs">
                <Row justify="between">
                  <Text size="xs" muted>
                    {mine ? '我' : m.role === 'assistant' ? who : m.role}
                  </Text>
                  <Row gap="xs">
                    {m.status && STATUS_LABEL[m.status] ? (
                      <Badge tone={m.status === 'failed' ? 'danger' : 'warning'}>{STATUS_LABEL[m.status]}</Badge>
                    ) : null}
                    {m.at ? <RelativeTime at={m.at} muted /> : null}
                  </Row>
                </Row>
                {mine ? <Text size="sm">{m.text || ' '}</Text> : <Markdown content={m.text || '（空）'} />}
              </Box>
            )
          })
        : null}
      {s.canSend ? (
        <Stack gap="xs">
          <Textarea
            label="回复"
            labelHidden
            rows={3}
            placeholder={`发消息给 ${who}`}
            value={read(draft)}
            onChange={(v) => {
              set(draft, v)
              set(clientId, null)
            }}
          />
          {problemCallout(read(sendProblem), '没发出去')}
          <Row justify="end">
            <Button tone="accent" icon="next" disabled={read(sending) || !read(draft).trim()} onPress={() => send(s)}>
              {read(sending) ? '发送中…' : '发送'}
            </Button>
          </Row>
        </Stack>
      ) : (
        <Text size="xs" muted>
          这个会话只能读，不能在这里回复。
        </Text>
      )}
    </Stack>
  )
}

export default function Panel() {
  const c = read(config)
  if (!c) return <Spinner label="正在加载…" />
  if (!c.hubUrl || !c.hasToken || read(editing)) return <Setup />
  const id = read(selected)
  const s = id ? read(overview)?.sessions.find((x) => x.id === id) : undefined
  return (
    <Stack gap="md">
      <Row justify="between">
        <Heading level={2}>Ensemble</Heading>
        <Row gap="none">
          <IconButton icon="refresh" label="刷新" variant="ghost" disabled={read(refreshing)} onPress={refresh} />
          <IconButton icon="settings" label="连接设置" variant="ghost" onPress={() => set(editing, true)} />
        </Row>
      </Row>
      {s ? <Thread s={s} /> : <AgentList />}
    </Stack>
  )
}
