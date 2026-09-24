import { apiClient, buildApiUrl } from '@/api/client'
import { ADMIN_UI_REQUEST_HEADER } from '@/api/adminUIRequest'
import type { FingerprintTarget, FingerprintTestEvent } from './types'

// 账号指纹测试的管理端点。路由注册在 backend/internal/server/routes/admin_kong_fingerprint.go。

/** 可选的目标模型。 */
export async function getFingerprintTargets(): Promise<FingerprintTarget[]> {
  const { data } = await apiClient.get<{ targets: FingerprintTarget[] | null }>('/admin/kong/fingerprint/targets')
  return data?.targets ?? []
}

/**
 * 发起测试时被拒绝（流还没开始）：400 目标模型不可测、404 账号不存在或不是 codex 协议的 OpenAI 账号、
 * 409 该账号已有测试在进行、503 指纹库不可用。message 取后端响应里的说明。
 */
export class FingerprintTestHttpError extends Error {
  readonly status: number

  constructor(status: number, message: string) {
    super(message)
    this.name = 'FingerprintTestHttpError'
    this.status = status
  }
}

/**
 * 从累积的缓冲里切出已收完的行并解析成事件，返回事件与还没收完的尾巴。
 *
 * 后端每条事件是一行 `data: <JSON>` 加一个空行；另有以 `:` 开头的保活注释。只认 `data:` 行，
 * 其余（注释、空行、其它字段）一律忽略；解析不出 JSON 的行也丢掉。
 */
export function parseSSEBuffer(buffer: string): { events: FingerprintTestEvent[]; rest: string } {
  const lines = buffer.split('\n')
  const rest = lines.pop() ?? ''
  const events: FingerprintTestEvent[] = []
  for (const line of lines) {
    const event = parseSSELine(line)
    if (event) events.push(event)
  }
  return { events, rest }
}

function parseSSELine(raw: string): FingerprintTestEvent | null {
  const line = raw.endsWith('\r') ? raw.slice(0, -1) : raw
  if (!line.startsWith('data:')) return null
  const payload = line.slice('data:'.length).trim()
  if (!payload) return null
  try {
    const event = JSON.parse(payload) as unknown
    if (event && typeof event === 'object' && typeof (event as { type?: unknown }).type === 'string') {
      return event as FingerprintTestEvent
    }
  } catch {
    // 不是 JSON：忽略这一行。
  }
  return null
}

export interface RunFingerprintTestOptions {
  /** 中止它即断开连接，后端随之停止剩余挑战。 */
  signal: AbortSignal
  onEvent: (event: FingerprintTestEvent) => void
}

/**
 * 对一个账号发起指纹测试，按 SSE 流逐条回调事件，流结束时返回。
 *
 * 用 fetch 而不是 EventSource：后者不支持 POST。被拒绝时抛 FingerprintTestHttpError；中止时抛 fetch
 * 自己的 AbortError。
 */
export async function runFingerprintTest(
  accountId: number,
  model: string,
  { signal, onEvent }: RunFingerprintTestOptions,
): Promise<void> {
  const response = await fetch(buildApiUrl(`/admin/accounts/${accountId}/kong-fingerprint-test`), {
    method: 'POST',
    headers: {
      Authorization: `Bearer ${localStorage.getItem('auth_token')}`,
      'Content-Type': 'application/json',
      [ADMIN_UI_REQUEST_HEADER]: '1',
    },
    body: JSON.stringify({ model }),
    signal,
  })
  if (!response.ok) {
    throw new FingerprintTestHttpError(response.status, await errorMessage(response))
  }

  const reader = response.body?.getReader()
  if (!reader) throw new Error('响应没有正文')
  const decoder = new TextDecoder()
  let buffer = ''
  for (;;) {
    const { done, value } = await reader.read()
    if (done) break
    buffer += decoder.decode(value, { stream: true })
    const parsed = parseSSEBuffer(buffer)
    buffer = parsed.rest
    parsed.events.forEach(onEvent)
  }
  // 流末尾没有换行时，最后一行也要处理。
  parseSSEBuffer(buffer + decoder.decode() + '\n').events.forEach(onEvent)
}

async function errorMessage(response: Response): Promise<string> {
  try {
    const body = (await response.json()) as { message?: unknown } | null
    if (body && typeof body.message === 'string' && body.message) return body.message
  } catch {
    // 不是 JSON 的错误页：退回状态码。
  }
  return `HTTP ${response.status}`
}
