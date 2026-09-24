import { beforeEach, describe, expect, it, vi } from 'vitest'
import { defineComponent } from 'vue'
import { mount } from '@vue/test-utils'

const { updateAccountMock, checkMixedChannelRiskMock } = vi.hoisted(() => ({
  updateAccountMock: vi.fn(),
  checkMixedChannelRiskMock: vi.fn()
}))

vi.mock('@/stores/app', () => ({
  useAppStore: () => ({ showError: vi.fn(), showSuccess: vi.fn(), showInfo: vi.fn() })
}))

vi.mock('@/stores/auth', () => ({
  useAuthStore: () => ({ isSimpleMode: true })
}))

vi.mock('@/api/admin', () => ({
  adminAPI: {
    accounts: { update: updateAccountMock, checkMixedChannelRisk: checkMixedChannelRiskMock },
    settings: {
      getWebSearchEmulationConfig: vi.fn().mockResolvedValue({ enabled: false, providers: [] }),
      getSettings: vi.fn().mockResolvedValue({})
    },
    tlsFingerprintProfiles: { list: vi.fn().mockResolvedValue([]) }
  }
}))

vi.mock('@/api/admin/accounts', () => ({
  getAntigravityDefaultModelMapping: vi.fn()
}))

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return { ...actual, useI18n: () => ({ t: (key: string) => key }) }
})

import EditAccountModal from '@/components/account/EditAccountModal.vue'

const BaseDialogStub = defineComponent({
  name: 'BaseDialog',
  props: { show: { type: Boolean, default: false } },
  template: '<div v-if="show"><slot /><slot name="footer" /></div>'
})

function buildOpenAIAccount(type: string, extra: Record<string, unknown>) {
  return {
    id: 7,
    name: 'OpenAI',
    notes: '',
    platform: 'openai',
    type,
    credentials: type === 'oauth' ? { access_token: 'oauth-token' } : { api_key: 'sk-test', base_url: 'https://api.openai.com' },
    extra,
    max_sessions: typeof extra.max_sessions === 'number' ? extra.max_sessions : undefined,
    proxy_id: null,
    concurrency: 8,
    priority: 1,
    rate_multiplier: 1,
    status: 'active',
    group_ids: [],
    expires_at: null,
    auto_pause_on_expired: false
  } as any
}

function mountModal(account: any) {
  return mount(EditAccountModal, {
    props: { show: true, account, proxies: [], groups: [] },
    global: {
      stubs: { BaseDialog: BaseDialogStub, Icon: true, ProxySelector: true, GroupSelector: true, ModelWhitelistSelector: true }
    }
  })
}

async function submit(wrapper: ReturnType<typeof mountModal>) {
  await wrapper.get('form#edit-account-form').trigger('submit.prevent')
  expect(updateAccountMock).toHaveBeenCalledTimes(1)
  return updateAccountMock.mock.calls[0]?.[1]
}

describe('EditAccountModal 的 OpenAI 会话数上限', () => {
  beforeEach(() => {
    updateAccountMock.mockReset().mockImplementation(async (_id: number, payload: any) => payload)
    checkMixedChannelRiskMock.mockReset().mockResolvedValue({ has_risk: false })
  })

  it('OpenAI OAuth 账号显示当前上限，改动后写回 extra', async () => {
    const wrapper = mountModal(buildOpenAIAccount('oauth', { max_sessions: 3 }))
    const input = wrapper.get<HTMLInputElement>('[data-testid="openai-max-sessions"]')
    expect(input.element.value).toBe('3')
    await input.setValue('5')
    const payload = await submit(wrapper)
    expect(payload.extra.max_sessions).toBe(5)
    expect(payload.extra.session_idle_timeout_minutes).toBe(15)
    wrapper.unmount()
  })

  it('清空即不限制', async () => {
    const wrapper = mountModal(buildOpenAIAccount('oauth', { max_sessions: 3, session_idle_timeout_minutes: 15 }))
    await wrapper.get('[data-testid="openai-max-sessions"]').setValue('')
    const payload = await submit(wrapper)
    expect(payload.extra).not.toHaveProperty('max_sessions')
    expect(payload.extra).not.toHaveProperty('session_idle_timeout_minutes')
    wrapper.unmount()
  })

  it('API Key 账号不显示', () => {
    const wrapper = mountModal(buildOpenAIAccount('apikey', {}))
    expect(wrapper.find('[data-testid="openai-max-sessions"]').exists()).toBe(false)
    wrapper.unmount()
  })
})
