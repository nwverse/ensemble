import { afterEach, assert, mockHost, mount, test, type TestPanel } from 'orca/testing'
import * as panel from './panel'
import type { Overview } from './protocol'

let view: TestPanel | undefined
afterEach(() => view?.unmount())

const ov: Overview = {
  fetchedAt: 1,
  accounts: [
    { accountId: 'a1', provider: 'gemini_tasks', displayName: 'Gemini Tasks · Ada', status: 'active' },
    { accountId: 'a2', provider: 'muse', displayName: 'Muse', status: 'active' }
  ],
  sessions: [
    { id: 'pac:s1', accountId: 'a1', provider: 'gemini_tasks', title: '旧金山天气', kind: 'task', canSend: true, unreadCount: 2, rankTimeMs: 1795681477385 },
    { id: 'pac:s2', accountId: 'a2', provider: 'muse', title: '主聊天', kind: 'main', canSend: true, unreadCount: 0, rankTimeMs: 1795681400000 }
  ]
}

test('asks for the hub and token before anything else', async () => {
  let saved: any = null
  const host = mockHost({
    requests: {
      'config.get': () => (saved ? { hubUrl: saved.hubUrl, hasToken: true } : { hubUrl: '', hasToken: false }),
      'config.save': (input) => {
        saved = input
        return { ok: true, value: { accounts: 2 } }
      },
      'overview.get': () => ({ ok: true, value: ov })
    }
  })
  view = await mount(panel, { host })
  assert.ok(view.find({ text: '连接 Persona' }))
  await view.type({ label: 'Hub 地址' }, 'https://hub.test')
  await view.type({ label: '设备凭据' }, 'tok')
  await view.press({ text: '保存并连接' })
  assert.deepEqual(saved, { hubUrl: 'https://hub.test', token: 'tok' })
  assert.ok(view.find({ text: 'Gemini Tasks · Ada' }))
  assert.deepEqual(view.errors, [])
  assert.deepEqual(view.refusals, [])
})

test('shows a refused token with what to do', async () => {
  view = await mount(panel, {
    host: mockHost({
      requests: {
        'config.get': () => ({ hubUrl: '', hasToken: false }),
        'config.save': () => ({ ok: false, code: 'unauthorized', message: '设备凭据无效或已被撤销' })
      }
    })
  })
  await view.type({ label: 'Hub 地址' }, 'https://hub.test')
  await view.type({ label: '设备凭据' }, 'bad')
  await view.press({ text: '保存并连接' })
  assert.ok(view.find({ text: '设备凭据无效或已被撤销' }))
  assert.ok(view.find({ text: '连接 Persona' }))
})

test('groups sessions by account, opens one, marks it read and replies', async () => {
  const sent: any[] = []
  const read: string[] = []
  let messages = [
    { id: 'u1', role: 'user', text: '天气？', at: 1, status: 'sent' },
    { id: 'a1', role: 'assistant', text: '**多云**', at: 2 }
  ]
  view = await mount(panel, {
    host: mockHost({
      requests: {
        'config.get': () => ({ hubUrl: 'https://hub.test', hasToken: true }),
        'overview.get': () => ({ ok: true, value: ov }),
        'messages.get': () => ({ ok: true, value: { messages, hasMore: false } }),
        'session.read': ({ id }) => {
          read.push(id)
          return { ok: true, value: { ok: true } }
        },
        'message.send': (input) => {
          sent.push(input)
          messages = [...messages, { id: 'u2', role: 'user', text: input.text, at: 3, status: 'pending' }]
          return { ok: true, value: { status: 'pending', duplicate: false } }
        }
      }
    })
  })
  assert.ok(view.find({ text: 'Gemini Tasks · Ada' }))
  assert.ok(view.find({ text: 'Muse' }))
  assert.ok(view.find({ text: '2 未读' }))
  await view.press({ text: /旧金山天气/ })
  assert.deepEqual(read, ['pac:s1'])
  assert.ok(view.find({ text: '天气？' }))
  await view.type({ label: '回复' }, '谢谢')
  await view.press({ text: '发送' })
  assert.equal(sent.length, 1)
  assert.equal(sent[0].id, 'pac:s1')
  assert.equal(sent[0].text, '谢谢')
  assert.match(sent[0].clientId, /^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/)
  assert.equal(view.valueOf({ label: '回复' }), '')
  assert.ok(view.find({ text: '排队中' }))
  await view.press({ text: '‹ 返回' })
  assert.ok(view.find({ text: 'Muse' }))
  assert.deepEqual(view.errors, [])
  assert.deepEqual(view.refusals, [])
})

test('a send that times out keeps its clientId for the retry', async () => {
  const ids: string[] = []
  let fail = true
  view = await mount(panel, {
    host: mockHost({
      requests: {
        'config.get': () => ({ hubUrl: 'https://hub.test', hasToken: true }),
        'overview.get': () => ({ ok: true, value: ov }),
        'messages.get': () => ({ ok: true, value: { messages: [], hasMore: false } }),
        'session.read': () => ({ ok: true, value: { ok: true } }),
        'message.send': (input) => {
          ids.push(input.clientId)
          if (fail) {
            fail = false
            throw Object.assign(new Error('timed out'), { code: 'timeout' })
          }
          return { ok: true, value: { status: 'pending', duplicate: true } }
        }
      }
    })
  })
  await view.press({ text: /主聊天/ })
  await view.type({ label: '回复' }, 'hi')
  await view.press({ text: '发送' })
  assert.ok(view.find({ text: /再按一次发送/ }))
  await view.press({ text: '发送' })
  assert.equal(ids.length, 2)
  assert.equal(ids[0], ids[1])
})

test('a draft stays with its conversation', async () => {
  const sent: any[] = []
  view = await mount(panel, {
    host: mockHost({
      requests: {
        'config.get': () => ({ hubUrl: 'https://hub.test', hasToken: true }),
        'overview.get': () => ({ ok: true, value: ov }),
        'messages.get': () => ({ ok: true, value: { messages: [], hasMore: false } }),
        'session.read': () => ({ ok: true, value: { ok: true } }),
        'message.send': (input) => {
          sent.push(input)
          if (sent.length === 1) throw Object.assign(new Error('timed out'), { code: 'timeout' })
          return { ok: true, value: { status: 'pending', duplicate: false } }
        }
      }
    })
  })
  await view.press({ text: /旧金山天气/ })
  await view.type({ label: '回复' }, '给 Gemini 的话')
  await view.press({ text: '发送' })
  await view.press({ text: '‹ 返回' })
  await view.press({ text: /主聊天/ })
  assert.equal(view.valueOf({ label: '回复' }), '')
  await view.type({ label: '回复' }, '给 Muse 的话')
  await view.press({ text: '发送' })
  assert.equal(sent[1].id, 'pac:s2')
  assert.equal(sent[1].text, '给 Muse 的话')
  assert.notEqual(sent[1].clientId, sent[0].clientId)
})
