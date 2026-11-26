// Talking to the Persona hub's personal-agent routes (/api/v1/agents…) with a scoped device token
// (scopes agents.read + agents.control). Only the worker imports this: panels have no network.
import type { Account, HubErrorCode, Message, Overview, Result, Session } from './protocol'

export type HubConfig = { hubUrl: string; token: string }

export class HubError extends Error {
  constructor(
    readonly code: HubErrorCode,
    message: string
  ) {
    super(message)
  }
}

const TIMEOUT_MS = 15_000
/** A components panel refuses text nodes past 16 KiB; a long reply is cut well before that. */
const MAX_TEXT = 6_000

/** The hub's base URL as typed, or null when it is not an http(s) URL. */
export function normalizeHubUrl(raw: string): string | null {
  const s = raw.trim().replace(/\/+$/, '')
  if (!s) return null
  try {
    const u = new URL(s)
    if (u.protocol !== 'https:' && u.protocol !== 'http:') return null
    return u.origin + u.pathname.replace(/\/+$/, '')
  } catch {
    return null
  }
}

export async function hubFetch(cfg: HubConfig, path: string, init: { method?: string; body?: unknown } = {}): Promise<any> {
  let res: Response
  try {
    res = await fetch(cfg.hubUrl + path, {
      method: init.method ?? 'GET',
      headers: {
        Authorization: `Bearer ${cfg.token}`,
        Accept: 'application/json',
        ...(init.body === undefined ? {} : { 'Content-Type': 'application/json' })
      },
      body: init.body === undefined ? undefined : JSON.stringify(init.body),
      signal: AbortSignal.timeout(TIMEOUT_MS)
    })
  } catch (err) {
    throw new HubError('unreachable', `连不上 ${cfg.hubUrl}：${(err as Error)?.message ?? err}`)
  }
  const text = await res.text()
  let data: any = null
  try {
    data = text ? JSON.parse(text) : null
  } catch {
    // not JSON: an HTML error page from a proxy, say
  }
  if (res.ok) return data
  const detail = (data && (data.error?.message ?? data.error ?? data.message)) || `HTTP ${res.status}`
  if (res.status === 401) throw new HubError('unauthorized', '设备凭据无效或已被撤销')
  if (res.status === 403) throw new HubError('forbidden', `设备凭据缺少权限（需要 agents.read 和 agents.control）：${detail}`)
  if (res.status === 404) throw new HubError('not_found', String(detail))
  throw new HubError('hub_error', String(detail))
}

export function fail<T>(err: unknown): Result<T> {
  if (err instanceof HubError) return { ok: false, code: err.code, message: err.message }
  return { ok: false, code: 'hub_error', message: String((err as Error)?.message ?? err) }
}

const sessionPath = (id: string) => `/api/v1/agents/sessions/${encodeURIComponent(id)}`

export async function fetchOverview(cfg: HubConfig, now: number): Promise<Overview> {
  const [agents, list] = await Promise.all([hubFetch(cfg, '/api/v1/agents'), hubFetch(cfg, '/api/v1/agents/sessions')])
  const accounts: Account[] = []
  for (const src of agents?.sources ?? []) {
    for (const a of src?.accounts ?? []) {
      if (!a?.accountId || a.status === 'removed') continue
      accounts.push({
        accountId: String(a.accountId),
        provider: String(a.provider ?? ''),
        displayName: String(a.displayName || a.provider || a.accountId),
        status: String(a.status ?? ''),
        ...(a.lastError ? { lastError: String(a.lastError) } : {})
      })
    }
  }
  const sessions: Session[] = (list?.sessions ?? [])
    .filter((s: any) => s?.id)
    .map((s: any) => ({
      id: String(s.id),
      accountId: String(s.accountId ?? ''),
      provider: String(s.provider ?? ''),
      title: String(s.title || '未命名会话'),
      kind: String(s.kind ?? ''),
      canSend: !!s.canSend,
      unreadCount: Number(s.unreadCount) || 0,
      rankTimeMs: Number(s.rankTimeMs) || 0
    }))
    .sort((a: Session, b: Session) => b.rankTimeMs - a.rankTimeMs)
  return { accounts, sessions, fetchedAt: now }
}

function clip(s: string): string {
  return s.length > MAX_TEXT ? s.slice(0, MAX_TEXT) + '…' : s
}

export function messageText(blocks: any[]): string {
  const parts: string[] = []
  for (const b of blocks ?? []) {
    if (!b) continue
    if (b.type === 'text' && b.text) parts.push(String(b.text))
    else if (b.type === 'tool-call') parts.push(`🔧 ${b.name ?? 'tool'}${b.summary ? `：${b.summary}` : ''}`)
    else if (b.summary || b.text) parts.push(String(b.summary || b.text))
  }
  return clip(parts.join('\n\n').trim())
}

export async function fetchMessages(cfg: HubConfig, id: string, limit = 40): Promise<{ messages: Message[]; hasMore: boolean }> {
  const page = await hubFetch(cfg, `${sessionPath(id)}/messages?limit=${limit}`)
  const messages: Message[] = (page?.messages ?? [])
    .map((m: any) => ({
      id: String(m.externalId || m.local?.clientId || `${m.position}`),
      role: String(m.role ?? ''),
      text: messageText(m.blocks),
      at: Number(m.createTimeMs) || 0,
      pos: Number(m.position) || 0,
      ...(m.local?.status ? { status: String(m.local.status) } : {})
    }))
    .sort((a: any, b: any) => a.pos - b.pos || a.at - b.at)
    .map(({ pos: _pos, ...m }: any) => m)
  return { messages, hasMore: !!page?.hasMore }
}

export async function sendMessage(cfg: HubConfig, id: string, text: string, clientId: string): Promise<{ status: string; duplicate: boolean }> {
  const res = await hubFetch(cfg, `${sessionPath(id)}/messages`, { method: 'POST', body: { clientId, text, origin: 'orca' } })
  return { status: String(res?.outbox?.status ?? 'pending'), duplicate: !!res?.duplicate }
}

export async function markRead(cfg: HubConfig, id: string): Promise<void> {
  await hubFetch(cfg, `${sessionPath(id)}/read`, { method: 'POST', body: {} })
}
