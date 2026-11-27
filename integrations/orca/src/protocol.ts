// The panel <-> worker contract, declared once: src/panel.tsx and src/worker.ts both read it.
export {}

export type Account = {
  accountId: string
  provider: string
  displayName: string
  status: string
  lastError?: string
}

export type Session = {
  /** "<source>:<sessionKey>", the id every per-session route takes (percent-encoded). */
  id: string
  accountId: string
  provider: string
  title: string
  kind: string
  canSend: boolean
  unreadCount: number
  rankTimeMs: number
}

export type Message = {
  id: string
  role: string
  text: string
  at: number
  /** Only on messages sent through Persona: pending | claimed | sent | unconfirmed | failed. */
  status?: string
}

/** Why a call to the hub failed, in words the panel can act on. */
export type HubErrorCode = 'not_configured' | 'unauthorized' | 'forbidden' | 'unreachable' | 'not_found' | 'hub_error'

export type Overview = {
  accounts: Account[]
  sessions: Session[]
  fetchedAt: number
}

export type Result<T> = { ok: true; value: T } | { ok: false; code: HubErrorCode; message: string }

declare module 'orca' {
  interface PluginRequests {
    'config.get': { output: { hubUrl: string; hasToken: boolean } }
    /** Saves what is given, then checks the hub answers with that token. */
    'config.save': { input: { hubUrl: string; token?: string }; output: Result<{ accounts: number }> }
    'config.clear': { output: { ok: true } }
    'overview.get': { input: { refresh?: boolean }; output: Result<Overview> }
    'messages.get': { input: { id: string }; output: Result<{ messages: Message[]; hasMore: boolean }> }
    /** `clientId` is a UUID the panel keeps for one message, so a retry is never a second send. */
    'message.send': { input: { id: string; text: string; clientId: string }; output: Result<{ status: string; duplicate: boolean }> }
    'session.read': { input: { id: string }; output: Result<{ ok: true }> }
  }
  interface PluginPanelEvents {
    /** Posted after each check of the hub, so open panels redraw without asking. */
    'overview.changed': Overview
  }
}
