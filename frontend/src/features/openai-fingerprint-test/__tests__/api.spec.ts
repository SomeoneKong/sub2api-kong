import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

const { getMock } = vi.hoisted(() => ({ getMock: vi.fn() }))

vi.mock('@/api/client', () => ({
  apiClient: { get: getMock },
  buildApiUrl: (path: string) => `/api/v1${path}`,
}))

import { FingerprintTestHttpError, getFingerprintTargets, parseSSEBuffer, runFingerprintTest } from '../api'
import type { FingerprintTestEvent } from '../types'

/** 一个按给定分块逐次吐出字节的响应体，用来模拟 SSE 在任意位置被切开。 */
function streamResponse(chunks: string[]) {
  const encoder = new TextEncoder()
  let i = 0
  return {
    ok: true,
    status: 200,
    body: {
      getReader: () => ({
        read: async () =>
          i < chunks.length ? { done: false, value: encoder.encode(chunks[i++]) } : { done: true, value: undefined },
      }),
    },
  }
}

function jsonErrorResponse(status: number, body: unknown) {
  return {
    ok: false,
    status,
    json: async () => body,
  }
}

const started = { type: 'started', test_id: 't1', target_model: 'gpt-5.5', max_parts: 3 }
const partStarted = { type: 'part_started', test_id: 't1', part: { index: 1, challenge_id: 'c1' } }
const part = {
  type: 'part',
  test_id: 't1',
  part: {
    index: 1,
    challenge_id: 'c1',
    status_code: 200,
    reported_model: 'gpt-5.5',
    digit_count: 120,
    valid: true,
    attribution: 'gpt-5.5',
    cumulative: [{ model: 'gpt-5.5', display_name: 'GPT-5.5', probability: 0.93 }],
  },
}
const done = {
  type: 'done',
  test_id: 't1',
  result: { execution: 'completed', end_reason: 'fingerprint_confident', verdict: 'match', parts: 1 },
}

describe('parseSSEBuffer', () => {
  it('只认 data 行，忽略保活注释与空行，未收完的尾巴留给下一次', () => {
    const buffer = `: keepalive\n\ndata: ${JSON.stringify(started)}\n\ndata: {"type":"par`
    const { events, rest } = parseSSEBuffer(buffer)
    expect(events).toEqual([started])
    expect(rest).toBe('data: {"type":"par')
  })

  it('兼容 CRLF 行尾与 data: 后不带空格', () => {
    const { events } = parseSSEBuffer(`data:${JSON.stringify(started)}\r\n\r\n`)
    expect(events).toEqual([started])
  })

  it('解析不出 JSON 或没有 type 的行被丢掉', () => {
    const { events } = parseSSEBuffer('data: not-json\n\ndata: {"foo":1}\n\ndata: \n\n')
    expect(events).toEqual([])
  })
})

describe('runFingerprintTest', () => {
  const fetchMock = vi.fn()

  beforeEach(() => {
    fetchMock.mockReset()
    vi.stubGlobal('fetch', fetchMock)
    localStorage.setItem('auth_token', 'tok')
  })

  afterEach(() => {
    vi.unstubAllGlobals()
    localStorage.removeItem('auth_token')
  })

  it('带鉴权与目标模型发 POST', async () => {
    fetchMock.mockResolvedValue(streamResponse([]))
    const controller = new AbortController()
    await runFingerprintTest(42, 'gpt-5.5', { signal: controller.signal, onEvent: () => {} })

    expect(fetchMock).toHaveBeenCalledTimes(1)
    const [url, init] = fetchMock.mock.calls[0]
    expect(url).toBe('/api/v1/admin/accounts/42/kong-fingerprint-test')
    expect(init.method).toBe('POST')
    expect(init.headers.Authorization).toBe('Bearer tok')
    expect(init.headers['Content-Type']).toBe('application/json')
    expect(JSON.parse(init.body)).toEqual({ model: 'gpt-5.5' })
    expect(init.signal).toBe(controller.signal)
  })

  it('事件被任意切块也能按顺序逐条回调，保活注释被忽略', async () => {
    const text = [
      `data: ${JSON.stringify(started)}\n\n`,
      ': keepalive\n\n',
      `data: ${JSON.stringify(partStarted)}\n\n`,
      `data: ${JSON.stringify(part)}\n\n`,
      `data: ${JSON.stringify(done)}\n\n`,
    ].join('')
    // 按 7 个字符切：行、`data:` 前缀与 JSON 都会被切在块中间。
    const chunks: string[] = []
    for (let i = 0; i < text.length; i += 7) chunks.push(text.slice(i, i + 7))
    fetchMock.mockResolvedValue(streamResponse(chunks))

    const events: FingerprintTestEvent[] = []
    await runFingerprintTest(1, 'gpt-5.5', { signal: new AbortController().signal, onEvent: (e) => events.push(e) })
    expect(events.map((e) => e.type)).toEqual(['started', 'part_started', 'part', 'done'])
    expect(events[2]).toEqual(part)
    expect(events[3]).toEqual(done)
  })

  it('多字节字符被切在块中间也能正确解码', async () => {
    const event = { ...done, result: { ...done.result, detail: '上游回报 gpt-5.4-mini' } }
    const bytes = new TextEncoder().encode(`data: ${JSON.stringify(event)}\n\n`)
    // 从一个汉字的中间切开。
    const cut = bytes.indexOf(0xe4) + 1
    const response = {
      ok: true,
      status: 200,
      body: {
        getReader: () => {
          const parts = [bytes.slice(0, cut), bytes.slice(cut)]
          let i = 0
          return {
            read: async () => (i < parts.length ? { done: false, value: parts[i++] } : { done: true, value: undefined }),
          }
        },
      },
    }
    fetchMock.mockResolvedValue(response)
    const events: FingerprintTestEvent[] = []
    await runFingerprintTest(1, 'gpt-5.5', { signal: new AbortController().signal, onEvent: (e) => events.push(e) })
    expect(events).toEqual([event])
  })

  it('流末尾没有换行时最后一条事件也不丢', async () => {
    fetchMock.mockResolvedValue(streamResponse([`data: ${JSON.stringify(done)}`]))
    const events: FingerprintTestEvent[] = []
    await runFingerprintTest(1, 'gpt-5.5', { signal: new AbortController().signal, onEvent: (e) => events.push(e) })
    expect(events).toEqual([done])
  })

  it('被拒绝时抛出带状态码与后端说明的错误', async () => {
    fetchMock.mockResolvedValue(jsonErrorResponse(409, { code: 409, message: '该账号已有指纹测试在进行' }))
    const err = await runFingerprintTest(1, 'gpt-5.5', {
      signal: new AbortController().signal,
      onEvent: () => {},
    }).catch((e: unknown) => e)
    expect(err).toBeInstanceOf(FingerprintTestHttpError)
    expect((err as FingerprintTestHttpError).status).toBe(409)
    expect((err as FingerprintTestHttpError).message).toBe('该账号已有指纹测试在进行')
  })

  it('错误响应不是 JSON 时退回状态码', async () => {
    fetchMock.mockResolvedValue({
      ok: false,
      status: 502,
      json: async () => {
        throw new SyntaxError('Unexpected token <')
      },
    })
    await expect(
      runFingerprintTest(1, 'gpt-5.5', { signal: new AbortController().signal, onEvent: () => {} }),
    ).rejects.toMatchObject({ status: 502, message: 'HTTP 502' })
  })
})

describe('getFingerprintTargets', () => {
  it('返回 targets；后端给 null 时返回空数组', async () => {
    getMock.mockResolvedValueOnce({ data: { targets: [{ model: 'gpt-5.5', display_name: 'GPT-5.5' }] } })
    expect(await getFingerprintTargets()).toEqual([{ model: 'gpt-5.5', display_name: 'GPT-5.5' }])
    expect(getMock).toHaveBeenLastCalledWith('/admin/kong/fingerprint/targets')

    getMock.mockResolvedValueOnce({ data: { targets: null } })
    expect(await getFingerprintTargets()).toEqual([])
  })
})
