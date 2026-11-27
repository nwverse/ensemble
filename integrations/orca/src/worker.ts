// Ensemble's worker: the only part with network. It keeps the hub URL in settings and the device
// token in secrets, answers the panel, and checks the hub on a timer so a reply raises a
// notification and the status bar count even with the panel closed (a worker with status bar
// items runs for as long as the plugin is enabled). With nothing configured it makes no request
// and sets no timer.
import type { OrcaWorkerApi } from 'orca/worker'
import { fail, fetchMessages, fetchOverview, HubError, markRead, normalizeHubUrl, sendMessage, type HubConfig } from './hub'
import type { Overview, Result } from './protocol'

const TOKEN = 'token'
/** Last unread count per session id; absent until the first check, which only records. */
const UNREAD = 'unread'

let timer: ReturnType<typeof setTimeout> | undefined
let stopped = false

async function readConfig(orca: OrcaWorkerApi): Promise<HubConfig | null> {
  const hubUrl = normalizeHubUrl(String((await orca.settings.get('hubUrl')) ?? ''))
  const token = await orca.secrets.get(TOKEN)
  return hubUrl && token ? { hubUrl, token } : null
}

async function needConfig(orca: OrcaWorkerApi): Promise<HubConfig> {
  const cfg = await readConfig(orca)
  if (!cfg) throw new HubError('not_configured', '还没连接 Persona hub')
  return cfg
}

function statusBar(orca: OrcaWorkerApi, fields: { text?: string; tone?: 'neutral' | 'accent' | 'warning'; tooltip?: string }): void {
  orca.host.call('statusBar.update', { id: 'unread', ...fields }).catch(() => {})
}

async function noteUnread(orca: OrcaWorkerApi, ov: Overview): Promise<void> {
  const before = (await orca.storage.get(UNREAD)) as Record<string, number> | null
  const now: Record<string, number> = {}
  for (const s of ov.sessions) now[s.id] = s.unreadCount
  await orca.storage.set(UNREAD, now)
  if (before && (await orca.settings.get('notify')) !== false) {
    const names = new Map(ov.accounts.map((a) => [a.accountId, a.displayName]))
    for (const s of ov.sessions) {
      const added = s.unreadCount - (before[s.id] ?? 0)
      if (added <= 0) continue
      const who = names.get(s.accountId) ?? s.provider
      void orca.host
        .call('notifications.show', { title: `${who} · ${s.title}`, body: `${added} 条新回复` })
        .catch(() => {})
    }
  }
  const total = ov.sessions.reduce((n, s) => n + s.unreadCount, 0)
  statusBar(orca, {
    text: total ? `Ensemble ${total}` : 'Ensemble',
    tone: total ? 'accent' : 'neutral',
    tooltip: total ? `Ensemble：${total} 条未读回复` : 'Ensemble：没有未读回复'
  })
}

async function check(orca: OrcaWorkerApi): Promise<Result<Overview>> {
  const cfg = await readConfig(orca)
  if (!cfg) {
    statusBar(orca, { text: 'Ensemble', tone: 'neutral', tooltip: 'Ensemble：在面板里连接 Persona hub' })
    return { ok: false, code: 'not_configured', message: '还没连接 Persona hub' }
  }
  try {
    const ov = await fetchOverview(cfg, Date.now())
    await noteUnread(orca, ov)
    orca.panels.post('overview.changed', ov)
    return { ok: true, value: ov }
  } catch (err) {
    const res = fail<Overview>(err)
    if (!res.ok) statusBar(orca, { text: 'Ensemble !', tone: 'warning', tooltip: `Ensemble：${res.message}` })
    return res
  }
}

async function schedule(orca: OrcaWorkerApi): Promise<void> {
  if (timer) clearTimeout(timer)
  timer = undefined
  if (stopped || !(await readConfig(orca))) return
  const secs = Math.max(10, Number(await orca.settings.get('pollSeconds')) || 30)
  timer = setTimeout(async () => {
    timer = undefined
    await check(orca)
    await schedule(orca)
  }, secs * 1000)
}

export default function activate(orca: OrcaWorkerApi): void {
  stopped = false

  orca.panels.handle('config.get', async () => ({
    hubUrl: String((await orca.settings.get('hubUrl')) ?? ''),
    hasToken: !!(await orca.secrets.get(TOKEN))
  }))

  orca.panels.handle('config.save', async ({ hubUrl, token }) => {
    const url = normalizeHubUrl(hubUrl)
    if (!url) return { ok: false, code: 'not_configured', message: 'hub 地址要以 https:// 或 http:// 开头' }
    const tok = token?.trim() || (await orca.secrets.get(TOKEN))
    if (!tok) return { ok: false, code: 'not_configured', message: '请填入设备凭据' }
    try {
      // Checked before anything is stored, so a typo never replaces a working setup.
      const ov = await fetchOverview({ hubUrl: url, token: tok }, Date.now())
      await orca.settings.set('hubUrl', url)
      if (token?.trim()) await orca.secrets.set(TOKEN, tok)
      await orca.storage.delete(UNREAD)
      await noteUnread(orca, ov)
      orca.panels.post('overview.changed', ov)
      await schedule(orca)
      return { ok: true, value: { accounts: ov.accounts.length } }
    } catch (err) {
      return fail(err)
    }
  })

  orca.panels.handle('config.clear', async () => {
    await orca.secrets.delete(TOKEN)
    await orca.storage.delete(UNREAD)
    await schedule(orca)
    statusBar(orca, { text: 'Ensemble', tone: 'neutral', tooltip: 'Ensemble：在面板里连接 Persona hub' })
    return { ok: true as const }
  })

  orca.panels.handle('overview.get', async () => check(orca))

  orca.panels.handle('messages.get', async ({ id }) => {
    try {
      return { ok: true, value: await fetchMessages(await needConfig(orca), id) }
    } catch (err) {
      return fail(err)
    }
  })

  orca.panels.handle('message.send', async ({ id, text, clientId }) => {
    if (!text.trim()) return { ok: false, code: 'hub_error', message: '消息是空的' }
    try {
      return { ok: true, value: await sendMessage(await needConfig(orca), id, text, clientId) }
    } catch (err) {
      return fail(err)
    }
  })

  orca.panels.handle('session.read', async ({ id }) => {
    try {
      await markRead(await needConfig(orca), id)
      // The count drops at once instead of at the next check.
      const seen = ((await orca.storage.get(UNREAD)) as Record<string, number> | null) ?? null
      if (seen) await orca.storage.set(UNREAD, { ...seen, [id]: 0 })
      return { ok: true, value: { ok: true as const } }
    } catch (err) {
      return fail(err)
    }
  })

  orca.commands.register('ensemble.refresh', async () => {
    const res = await check(orca)
    if (!res.ok) return res.message
    const unread = res.value.sessions.reduce((n, s) => n + s.unreadCount, 0)
    return `${res.value.accounts.length} 个账号，${unread} 条未读`
  })

  orca.settings.onDidChange(({ key }) => {
    if (key === 'hubUrl' || key === 'pollSeconds') void schedule(orca)
  })

  // First check now (records the unread baseline), then on the timer.
  void readConfig(orca).then(async (cfg) => {
    if (!cfg || stopped) return
    await check(orca)
    await schedule(orca)
  })
}

export function deactivate(): void {
  stopped = true
  if (timer) clearTimeout(timer)
  timer = undefined
}
