/**
 * OpenAI OAuth 账号的会话数上限（设计见 DESIGN-openai-session-limit.md）。
 *
 * 上限存在账号 extra 的 max_sessions，空即不限制。设置上限时一并写入空闲超时 15 分钟（若 extra 里还没有）：
 * 会话只在请求开始时登记，空闲超时必须覆盖单个请求的时长，上游默认的 5 分钟会把长请求中途算作会话结束。
 */
export const OPENAI_SESSION_IDLE_TIMEOUT_MINUTES = 15

interface AccountKind {
  platform?: string | null
  type?: string | null
}

export function isOpenAIOAuthAccount(account: AccountKind | null | undefined): boolean {
  return account?.platform === 'openai' && account?.type === 'oauth'
}

/** 输入框的值：空、非数字、小于 1 都当作不限制；小数取整。 */
export function normalizeMaxSessions(value: unknown): number | null {
  if (value === null || value === undefined || value === '') {
    return null
  }
  const n = Math.floor(Number(value))
  return Number.isFinite(n) && n >= 1 ? n : null
}

function readMaxSessions(extra: Record<string, unknown> | null | undefined): number | null {
  return normalizeMaxSessions(extra?.max_sessions)
}

/**
 * 保存时调用：上限有改动才把它合并进 updatePayload.extra，避免用弹窗打开时的 extra 快照覆盖运行中写入的键。
 * extra 以 updatePayload 里已经组装好的为底，没有时用账号当前的。
 */
export function applyOpenAISessionLimit(
  updatePayload: Record<string, unknown>,
  account: (AccountKind & { extra?: Record<string, unknown> | null }) | null | undefined,
  maxSessions: number | null
): void {
  if (!account || !isOpenAIOAuthAccount(account)) {
    return
  }
  const next = normalizeMaxSessions(maxSessions)
  if (next === readMaxSessions(account.extra)) {
    return
  }
  const base = (updatePayload.extra as Record<string, unknown> | undefined) ?? account.extra ?? {}
  const extra: Record<string, unknown> = { ...base }
  if (next === null) {
    delete extra.max_sessions
    delete extra.session_idle_timeout_minutes
  } else {
    extra.max_sessions = next
    if (normalizeMaxSessions(extra.session_idle_timeout_minutes) === null) {
      extra.session_idle_timeout_minutes = OPENAI_SESSION_IDLE_TIMEOUT_MINUTES
    }
  }
  updatePayload.extra = extra
}
