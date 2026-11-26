// `orca plugin check` renders the panel once per scenario. The hub is stubbed: no scenario reaches the network.
import { defineCheck } from 'orca/testing'

const ov = {
  fetchedAt: 1,
  accounts: [
    { accountId: 'a1', provider: 'gemini_tasks', displayName: 'Gemini Tasks · Ada', status: 'active' },
    { accountId: 'a2', provider: 'muse', displayName: 'Muse · Glint', status: 'active' },
    { accountId: 'a3', provider: 'grok_bot', displayName: 'Grok Bot', status: 'active', lastError: 'NETWORK_ERROR' }
  ],
  sessions: [
    { id: 'pac:s1', accountId: 'a1', provider: 'gemini_tasks', title: '旧金山实时天气查询', kind: 'task', canSend: true, unreadCount: 2, rankTimeMs: 1795681477385 },
    { id: 'pac:s2', accountId: 'a2', provider: 'muse', title: '主聊天', kind: 'main', canSend: true, unreadCount: 0, rankTimeMs: 1795681400000 },
    { id: 'pac:s3', accountId: 'a2', provider: 'muse', title: '解读推特链接内容', kind: 'side', canSend: true, unreadCount: 1, rankTimeMs: 1795681300000 }
  ]
}

const connected = {
  'config.get': () => ({ hubUrl: 'https://persona.example.ts.net', hasToken: true }),
  'overview.get': () => ({ ok: true, value: ov })
}

export default defineCheck({
  setup: {},
  agents: { requests: connected },
  thread: {
    persisted: { plugin: { selected: 'pac:s1' } },
    requests: {
      ...connected,
      'session.read': () => ({ ok: true, value: { ok: true } }),
      'messages.get': () => ({
        ok: true,
        value: {
          hasMore: false,
          messages: [
            { id: 'u1', role: 'user', text: '查一下今天旧金山的天气', at: 1795681464152, status: 'sent' },
            { id: 'a1', role: 'assistant', text: '今天**多云**，最高 18°C，傍晚有雾。', at: 1795681464153 },
            { id: 'u2', role: 'user', text: '明天呢？', at: 1795681477000, status: 'pending' }
          ]
        }
      })
    }
  },
  unreachable: {
    requests: {
      'config.get': () => ({ hubUrl: 'https://persona.example.ts.net', hasToken: true }),
      'overview.get': () => ({ ok: false, code: 'unreachable', message: '连不上 https://persona.example.ts.net：fetch failed' })
    }
  }
})
