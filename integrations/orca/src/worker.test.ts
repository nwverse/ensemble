// The worker against a stub hub: globalThis.fetch answers like Persona's /api/v1/agents routes.
import { afterEach, assert, mockHost, mountWorker, test } from 'orca/testing'
import * as main from './worker'

type Call = { url: string; method: string; auth: string | null; body: any }
let calls: Call[] = []
const realFetch = globalThis.fetch

function stubHub(routes: Record<string, (body: any) => { status?: number; json: unknown }>): void {
  calls = []
  globalThis.fetch = (async (input: any, init: any = {}) => {
    const url = String(input)
    const method = init.method ?? 'GET'
    const body = init.body ? JSON.parse(init.body) : undefined
    calls.push({ url, method, auth: new Headers(init.headers).get('authorization'), body })
    const path = new URL(url).pathname + new URL(url).search
    const route = routes[`${method} ${path}`]
    if (!route) return new Response('{"error":"no route"}', { status: 404 })
    const { status = 200, json } = route(body)
    return new Response(JSON.stringify(json), { status, headers: { 'content-type': 'application/json' } })
  }) as typeof fetch
}

afterEach(() => {
  globalThis.fetch = realFetch
})

const SID = 'pac:agent:gemini_tasks:a864:8857'
const agents = {
  sources: [
    {
      source: 'pac',
      enabled: true,
      providers: [],
      accounts: [
        { accountId: 'a864', provider: 'gemini_tasks', displayName: 'Gemini Tasks · Ada', status: 'active', sessionCount: 1 },
        { accountId: 'a431', provider: 'muse2api', displayName: 'Muse', status: 'removed', sessionCount: 1 }
      ]
    }
  ]
}
const sessions = (unread: number) => ({
  sessions: [
    { id: SID, source: 'pac', sessionKey: 'agent:gemini_tasks:a864:8857', accountId: 'a864', provider: 'gemini_tasks', title: '旧金山天气', kind: 'task', canSend: true, unreadCount: unread, rankTimeMs: 20 },
    { id: 'pac:old', accountId: 'a864', provider: 'gemini_tasks', title: '', kind: 'task', canSend: false, unreadCount: 0, rankTimeMs: 10 }
  ]
})

const configured = () =>
  mockHost({
    capabilities: ['storage:own', 'secrets:own', 'settings:own', 'notifications:show'],
    settings: { hubUrl: 'https://hub.test', notify: true, pollSeconds: 600 },
    secrets: { token: 'tok-1' }
  })

test('makes no request until a hub is connected', async () => {
  stubHub({})
  const worker = await mountWorker(main, { host: mockHost({ capabilities: ['storage:own', 'secrets:own', 'settings:own', 'notifications:show'] }) })
  const res = await worker.request('overview.get', {})
  assert.equal(res.ok, false)
  assert.equal(!res.ok && res.code, 'not_configured')
  assert.equal(calls.length, 0)
  assert.deepEqual(worker.refusals, [])
  await worker.deactivate()
})

test('checks the token before saving it, and refuses a bad one', async () => {
  stubHub({
    'GET /api/v1/agents': () => ({ status: 401, json: { error: 'unauthorized' } }),
    'GET /api/v1/agents/sessions': () => ({ status: 401, json: { error: 'unauthorized' } })
  })
  const host = mockHost({ capabilities: ['storage:own', 'secrets:own', 'settings:own', 'notifications:show'] })
  const worker = await mountWorker(main, { host })
  const bad = await worker.request('config.save', { hubUrl: 'https://hub.test/', token: 'nope' })
  assert.equal(!bad.ok && bad.code, 'unauthorized')
  assert.equal(host.data().secrets.token, undefined)

  stubHub({ 'GET /api/v1/agents': () => ({ json: agents }), 'GET /api/v1/agents/sessions': () => ({ json: sessions(0) }) })
  const good = await worker.request('config.save', { hubUrl: 'https://hub.test/', token: ' tok-2 ' })
  assert.deepEqual(good, { ok: true, value: { accounts: 1 } })
  assert.equal(host.data().secrets.token, 'tok-2')
  assert.equal(host.data().settings.hubUrl, 'https://hub.test')
  assert.equal(calls[0].auth, 'Bearer tok-2')
  assert.deepEqual(await worker.request('config.get'), { hubUrl: 'https://hub.test', hasToken: true })
  await worker.deactivate()
})

test('lists accounts and sessions, and notifies only on new replies after the first check', async () => {
  let unread = 1
  stubHub({ 'GET /api/v1/agents': () => ({ json: agents }), 'GET /api/v1/agents/sessions': () => ({ json: sessions(unread) }) })
  const host = configured()
  const worker = await mountWorker(main, { host })
  const first = await worker.request('overview.get', {})
  assert.ok(first.ok)
  if (!first.ok) return
  assert.deepEqual(first.value.accounts.map((a) => a.accountId), ['a864'])
  assert.deepEqual(first.value.sessions.map((s) => [s.id, s.title]), [[SID, '旧金山天气'], ['pac:old', '未命名会话']])
  // Unread already there when Ensemble starts is recorded, not announced.
  assert.equal(host.callsOf('notifications.show').length, 0)

  unread = 3
  await worker.request('overview.get', {})
  const shown = host.callsOf('notifications.show')
  assert.equal(shown.length, 1)
  assert.deepEqual(shown[0], { title: 'Gemini Tasks · Ada · 旧金山天气', body: '2 条新回复' })
  assert.ok(worker.posts.some((p) => p.event === 'overview.changed'))
  const bar = host.callsOf('statusBar.update').at(-1) as any
  assert.equal(bar.text, 'Ensemble 3')
  assert.deepEqual(worker.refusals, [])
  await worker.deactivate()
})

test('reads, sends with the panel’s clientId and marks read on the percent-encoded session id', async () => {
  const enc = encodeURIComponent(SID)
  stubHub({
    [`GET /api/v1/agents/sessions/${enc}/messages?limit=40`]: () => ({
      json: {
        messages: [
          { externalId: 'rc_1', role: 'assistant', blocks: [{ type: 'text', text: '多云，18°C' }, { type: 'tool-call', name: 'search', summary: 'weather sf' }], createTimeMs: 2, position: 2 },
          { externalId: 'r_1:u', role: 'user', blocks: [{ type: 'text', text: '天气？' }], createTimeMs: 1, position: 1, local: { clientId: 'c', status: 'sent' } }
        ],
        hasMore: false
      }
    }),
    [`POST /api/v1/agents/sessions/${enc}/messages`]: (body) => ({ json: { outbox: { clientId: body.clientId, status: 'pending' }, duplicate: false } }),
    [`POST /api/v1/agents/sessions/${enc}/read`]: () => ({ json: { ok: true } })
  })
  const worker = await mountWorker(main, { host: configured() })
  const page = await worker.request('messages.get', { id: SID })
  assert.ok(page.ok)
  if (!page.ok) return
  assert.deepEqual(page.value.messages.map((m) => [m.id, m.role, m.text, m.status]), [
    ['r_1:u', 'user', '天气？', 'sent'],
    ['rc_1', 'assistant', '多云，18°C\n\n🔧 search：weather sf', undefined]
  ])
  const clientId = '7b0c2f7e-5a43-4d1b-9a55-0f3a9c1d2e4f'
  assert.deepEqual(await worker.request('message.send', { id: SID, text: '谢谢', clientId }), { ok: true, value: { status: 'pending', duplicate: false } })
  const sent = calls.find((c) => c.method === 'POST' && c.url.endsWith('/messages'))
  assert.deepEqual(sent?.body, { clientId, text: '谢谢', origin: 'orca' })
  assert.deepEqual(await worker.request('session.read', { id: SID }), { ok: true, value: { ok: true } })
  const missing = await worker.request('messages.get', { id: 'pac:gone' })
  assert.equal(!missing.ok && missing.code, 'not_found')
  await worker.deactivate()
})

test('says when the hub cannot be reached', async () => {
  globalThis.fetch = (async () => {
    throw new TypeError('fetch failed')
  }) as typeof fetch
  const worker = await mountWorker(main, { host: configured() })
  const res = await worker.request('overview.get', {})
  assert.equal(!res.ok && res.code, 'unreachable')
  await worker.deactivate()
})
