import { afterEach, describe, expect, it } from 'vitest'
import { mount } from '@vue/test-utils'

import FingerprintTestMenuItem from '../FingerprintTestMenuItem.vue'
import { closeFingerprintTest, fingerprintTestAccount } from '../state'
import type { Account } from '@/types'

function makeAccount(overrides: Partial<Account>): Account {
  return { id: 7, name: 'acc', platform: 'openai', type: 'oauth', ...overrides } as Account
}

describe('FingerprintTestMenuItem', () => {
  afterEach(closeFingerprintTest)

  it('只对 OpenAI 平台的 oauth / setup-token 账号显示，影子账号与 Agent Identity 账号除外', () => {
    const cases: Array<[Partial<Account>, boolean]> = [
      [{ platform: 'openai', type: 'oauth' }, true],
      [{ platform: 'openai', type: 'oauth', parent_account_id: null }, true],
      [{ platform: 'openai', type: 'setup-token' }, true],
      [{ platform: 'openai', type: 'oauth', parent_account_id: 3 }, false],
      [{ platform: 'openai', type: 'oauth', credentials: { auth_mode: 'agentIdentity' } }, false],
      [{ platform: 'openai', type: 'oauth', credentials: { auth_mode: 'chatgpt' } }, true],
      [{ platform: 'openai', type: 'apikey' }, false],
      [{ platform: 'anthropic', type: 'oauth' }, false],
      [{ platform: 'anthropic', type: 'setup-token' }, false],
    ]
    for (const [overrides, visible] of cases) {
      const wrapper = mount(FingerprintTestMenuItem, { props: { account: makeAccount(overrides) } })
      expect(wrapper.find('button').exists()).toBe(visible)
      wrapper.unmount()
    }
  })

  it('点击后记下要测试的账号并请求关闭菜单', async () => {
    const account = makeAccount({})
    const wrapper = mount(FingerprintTestMenuItem, { props: { account } })
    await wrapper.find('button').trigger('click')
    expect(fingerprintTestAccount.value).toStrictEqual(account)
    expect(wrapper.emitted('close')).toHaveLength(1)
    wrapper.unmount()
  })
})
