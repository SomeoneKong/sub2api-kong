import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'

import type { Account } from '@/types'
import type { FingerprintTestEvent } from '../types'

const { getTargetsMock, runMock } = vi.hoisted(() => ({ getTargetsMock: vi.fn(), runMock: vi.fn() }))

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return { ...actual, useI18n: () => ({ t: (key: string) => key }) }
})

vi.mock('../api', async () => {
  const actual = await vi.importActual<typeof import('../api')>('../api')
  return { ...actual, getFingerprintTargets: getTargetsMock, runFingerprintTest: runMock }
})

import FingerprintTestHost from '../FingerprintTestHost.vue'
import { closeFingerprintTest, fingerprintTestAccount, openFingerprintTest } from '../state'

const account = { id: 7, name: 'acc-7', platform: 'openai', type: 'oauth' } as Account
const bodyText = () => document.body.textContent ?? ''
const testIdText = (id: string) => document.body.querySelector(`[data-testid="${id}"]`)?.textContent?.trim()
const startButton = () =>
  Array.from(document.body.querySelectorAll('button')).find((b) => /开始|重新测试/.test(b.textContent ?? ''))

type RunOptions = { signal: AbortSignal; onEvent: (e: FingerprintTestEvent) => void }

describe('FingerprintTestHost', () => {
  beforeEach(() => {
    getTargetsMock.mockResolvedValue([
      { model: 'gpt-6.1-sol', display_name: 'gpt-6.1-sol' },
      { model: 'gpt-6-astra', display_name: 'gpt-6-astra' },
    ])
    runMock.mockReset()
  })

  afterEach(() => {
    closeFingerprintTest()
    document.body.innerHTML = ''
  })

  it('状态为空时不显示弹窗，设置账号后显示', async () => {
    const wrapper = mount(FingerprintTestHost, { attachTo: document.body })
    expect(bodyText()).not.toContain('指纹测试')

    openFingerprintTest(account)
    await flushPromises()
    expect(bodyText()).toContain('指纹测试')
    expect(bodyText()).toContain('acc-7')
    expect(bodyText()).toContain('不是绝对档位判定')
    expect(bodyText()).toContain('指纹库以外的模型也会被归到最像的候选上')
    expect(bodyText()).toContain('只测 low 推理强度')
    expect(bodyText()).toContain('不保证落到同一个模型')
    wrapper.unmount()
  })

  it('默认选第一个目标，逐份显示证据，执行结果与模型结论分开显示', async () => {
    runMock.mockImplementation(async (_id: number, _model: string, { onEvent }: RunOptions) => {
      const group = { model: 'gpt-6-astra|gpt-6.1-sol', display_name: 'gpt-6-astra / gpt-6.1-sol', probability: 0.999 }
      const pair = [
        { model: 'gpt-6.1-sol', display_name: 'gpt-6.1-sol', probability: 0.9986 },
        { model: 'gpt-6-astra', display_name: 'gpt-6-astra', probability: 0.0014 },
      ]
      onEvent({ type: 'started', test_id: 'tid-1', target_model: 'gpt-6.1-sol', max_parts: 4 })
      onEvent({ type: 'part_started', test_id: 'tid-1', part: { index: 1, challenge_id: 'text-packed-v1' } })
      onEvent({
        type: 'part',
        test_id: 'tid-1',
        part: {
          index: 1,
          challenge_id: 'text-packed-v1',
          status_code: 200,
          reported_model: 'gpt-6.1-sol-2026-01-01',
          section_count: 9,
          valid: true,
          attribution: group.model,
          cumulative: [group],
          pair: [
            { model: 'gpt-6.1-sol', display_name: 'gpt-6.1-sol', probability: 0.9634 },
            { model: 'gpt-6-astra', display_name: 'gpt-6-astra', probability: 0.0366 },
          ],
        },
      })
      onEvent({ type: 'part_started', test_id: 'tid-1', part: { index: 2, challenge_id: 'text-packed-v1' } })
      onEvent({
        type: 'part',
        test_id: 'tid-1',
        part: {
          index: 2,
          challenge_id: 'text-packed-v1',
          status_code: 200,
          reported_model: 'gpt-6.1-sol',
          section_count: 10,
          valid: true,
          attribution: group.model,
          cumulative: [group],
          pair,
        },
      })
      onEvent({
        type: 'done',
        test_id: 'tid-1',
        result: {
          execution: 'completed',
          end_reason: 'fingerprint_confident',
          detail: '指纹判为 gpt-6.1-sol',
          verdict: 'match',
          decided: 'gpt-6.1-sol',
          parts: 2,
          candidates: [group],
          pair,
          persist_error: 'db down',
        },
      })
    })

    openFingerprintTest(account)
    const wrapper = mount(FingerprintTestHost, { attachTo: document.body })
    await flushPromises()

    startButton()!.click()
    await flushPromises()

    expect(runMock).toHaveBeenCalledTimes(1)
    expect(runMock.mock.calls[0][0]).toBe(7)
    expect(runMock.mock.calls[0][1]).toBe('gpt-6.1-sol')

    const text = bodyText()
    expect(text).toContain('第 1 份')
    expect(text).toContain('gpt-6.1-sol-2026-01-01')
    expect(text).toContain('第 2 份')
    expect(text).toContain('96.3%')
    expect(text).toContain('gpt-6-astra / gpt-6.1-sol')
    expect(text).toContain('两者细分')
    expect(text).toContain('99.9%')
    expect(text).toContain('0.1%')
    expect(text).toContain('相近两者的细分')
    expect(text).toContain('指纹判为 gpt-6.1-sol')
    expect(testIdText('fp-execution')).toBe('完成')
    expect(testIdText('fp-verdict')).toBe('一致')
    expect(text).toContain('累计指纹归因达到 0.95，且各份归因一致')
    expect(text).toContain('有证据没写进库')
    expect(text).toContain('tid-1')
    wrapper.unmount()
  })

  it('执行失败时没有模型结论', async () => {
    runMock.mockImplementation(async (_id: number, _model: string, { onEvent }: RunOptions) => {
      onEvent({ type: 'started', test_id: 'tid-2', target_model: 'gpt-6.1-sol', max_parts: 4 })
      onEvent({
        type: 'done',
        test_id: 'tid-2',
        result: { execution: 'failed', end_reason: 'proxy_unavailable', detail: 'no proxy', parts: 0 },
      })
    })

    openFingerprintTest(account)
    const wrapper = mount(FingerprintTestHost, { attachTo: document.body })
    await flushPromises()
    startButton()!.click()
    await flushPromises()

    const text = bodyText()
    expect(testIdText('fp-execution')).toBe('失败')
    expect(testIdText('fp-verdict')).toBe('无（执行未完成）')
    expect(text).not.toContain('判不准')
    expect(text).toContain('账号的代理不可用')
    wrapper.unmount()
  })

  it('关闭弹窗时中止进行中的请求并清空状态', async () => {
    let signal: AbortSignal | undefined
    runMock.mockImplementation(
      (_id: number, _model: string, opts: RunOptions) =>
        new Promise<void>((_resolve, reject) => {
          signal = opts.signal
          opts.signal.addEventListener('abort', () => reject(new DOMException('aborted', 'AbortError')))
        }),
    )

    openFingerprintTest(account)
    const wrapper = mount(FingerprintTestHost, { attachTo: document.body })
    await flushPromises()
    startButton()!.click()
    await flushPromises()
    expect(signal?.aborted).toBe(false)

    const closeButton = Array.from(document.body.querySelectorAll('button')).find((b) => b.textContent?.trim() === '关闭')
    closeButton!.click()
    await flushPromises()

    expect(signal?.aborted).toBe(true)
    expect(fingerprintTestAccount.value).toBeNull()
    expect(bodyText()).not.toContain('指纹测试')
    wrapper.unmount()
  })
})
