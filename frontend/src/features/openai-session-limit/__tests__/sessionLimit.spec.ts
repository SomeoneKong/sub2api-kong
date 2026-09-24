import { describe, expect, it } from 'vitest'
import {
  OPENAI_SESSION_IDLE_TIMEOUT_MINUTES,
  applyOpenAISessionLimit,
  isOpenAIOAuthAccount,
  normalizeMaxSessions
} from '../sessionLimit'

const oauth = (extra?: Record<string, unknown>) => ({ platform: 'openai', type: 'oauth', extra })

describe('isOpenAIOAuthAccount', () => {
  it('只认 OpenAI OAuth', () => {
    expect(isOpenAIOAuthAccount(oauth())).toBe(true)
    expect(isOpenAIOAuthAccount({ platform: 'openai', type: 'apikey' })).toBe(false)
    expect(isOpenAIOAuthAccount({ platform: 'anthropic', type: 'oauth' })).toBe(false)
    expect(isOpenAIOAuthAccount(null)).toBe(false)
  })
})

describe('normalizeMaxSessions', () => {
  it('空与无效值即不限制，小数取整', () => {
    expect(normalizeMaxSessions('')).toBeNull()
    expect(normalizeMaxSessions(null)).toBeNull()
    expect(normalizeMaxSessions('abc')).toBeNull()
    expect(normalizeMaxSessions(0)).toBeNull()
    expect(normalizeMaxSessions(-2)).toBeNull()
    expect(normalizeMaxSessions('3')).toBe(3)
    expect(normalizeMaxSessions(2.7)).toBe(2)
  })
})

describe('applyOpenAISessionLimit', () => {
  it('设置上限时写入上限与空闲超时，保留其他键', () => {
    const payload: Record<string, unknown> = {}
    applyOpenAISessionLimit(payload, oauth({ codex_7d_used_percent: 12 }), 3)
    expect(payload.extra).toEqual({
      codex_7d_used_percent: 12,
      max_sessions: 3,
      session_idle_timeout_minutes: OPENAI_SESSION_IDLE_TIMEOUT_MINUTES
    })
  })

  it('已有空闲超时时不覆盖', () => {
    const payload: Record<string, unknown> = {}
    applyOpenAISessionLimit(payload, oauth({ session_idle_timeout_minutes: 30 }), 2)
    expect(payload.extra).toEqual({ max_sessions: 2, session_idle_timeout_minutes: 30 })
  })

  it('清空时删除两个键', () => {
    const payload: Record<string, unknown> = {}
    applyOpenAISessionLimit(payload, oauth({ max_sessions: 3, session_idle_timeout_minutes: 15, other: 1 }), null)
    expect(payload.extra).toEqual({ other: 1 })
  })

  it('没有改动时不碰 extra', () => {
    const payload: Record<string, unknown> = {}
    applyOpenAISessionLimit(payload, oauth({ max_sessions: 3 }), 3)
    expect(payload).not.toHaveProperty('extra')
    applyOpenAISessionLimit(payload, oauth(), null)
    expect(payload).not.toHaveProperty('extra')
  })

  it('以已组装的 extra 为底', () => {
    const payload: Record<string, unknown> = { extra: { upstream_request_id_header: 'x-req' } }
    applyOpenAISessionLimit(payload, oauth({ stale: true }), 1)
    expect(payload.extra).toEqual({
      upstream_request_id_header: 'x-req',
      max_sessions: 1,
      session_idle_timeout_minutes: OPENAI_SESSION_IDLE_TIMEOUT_MINUTES
    })
  })

  it('非 OpenAI OAuth 账号不处理', () => {
    const payload: Record<string, unknown> = {}
    applyOpenAISessionLimit(payload, { platform: 'anthropic', type: 'oauth', extra: {} }, 3)
    expect(payload).not.toHaveProperty('extra')
  })
})
