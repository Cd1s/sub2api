import { beforeEach, describe, expect, it, vi } from 'vitest'
import { defineComponent } from 'vue'
import { mount } from '@vue/test-utils'

// fork 私有：「加入号池动态调度」开关（credentials.ours_tiering）

const { updateAccountMock, authIsSimpleMode } = vi.hoisted(() => ({
  updateAccountMock: vi.fn(),
  authIsSimpleMode: { value: true }
}))

vi.mock('@/stores/app', () => ({
  useAppStore: () => ({ showError: vi.fn(), showSuccess: vi.fn(), showInfo: vi.fn() })
}))
vi.mock('@/stores/auth', () => ({ useAuthStore: () => ({ get isSimpleMode() { return authIsSimpleMode.value } }) }))
vi.mock('@/api/admin', () => ({
  adminAPI: {
    accounts: {
      update: updateAccountMock,
      checkMixedChannelRisk: vi.fn().mockResolvedValue({ has_risk: false })
    },
    settings: {
      getWebSearchEmulationConfig: vi.fn().mockResolvedValue({ enabled: false, providers: [] }),
      getSettings: vi.fn().mockResolvedValue({})
    },
    tlsFingerprintProfiles: { list: vi.fn().mockResolvedValue([]) }
  }
}))
vi.mock('@/api/admin/accounts', () => ({ getAntigravityDefaultModelMapping: vi.fn() }))
vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return { ...actual, useI18n: () => ({ t: (key: string) => key }) }
})

import EditAccountModal from '../EditAccountModal.vue'

const BaseDialogStub = defineComponent({
  props: { show: { type: Boolean, default: false } },
  template: '<div v-if="show"><slot /><slot name="footer" /></div>'
})

const account = (
  platform = 'openai',
  type = 'oauth',
  credentials: Record<string, unknown> = {},
  extra: Record<string, unknown> = {}
) => ({
  id: 21, name: 'Pool', notes: '', platform, type,
  credentials: { token_type: 'Bearer', model_mapping: { 'gpt-5.6-sol': 'gpt-5.6-sol' }, ...credentials },
  credentials_status: { has_access_token: true, has_refresh_token: true }, extra,
  proxy_id: null, concurrency: 1, priority: 50, rate_multiplier: 1, status: 'active',
  group_ids: [], expires_at: null, auto_pause_on_expired: false, parent_account_id: null
})

function mountModal(value: Record<string, unknown>) {
  return mount(EditAccountModal, {
    props: { show: true, account: value, proxies: [], groups: [] },
    global: { stubs: {
      BaseDialog: BaseDialogStub, Select: true, Icon: true, ProxySelector: true,
      GroupSelector: true, ModelWhitelistSelector: true
    } }
  })
}

async function submit(wrapper: ReturnType<typeof mountModal>) {
  await wrapper.get('form#edit-account-form').trigger('submit.prevent')
  expect(updateAccountMock).toHaveBeenCalledTimes(1)
  return updateAccountMock.mock.calls[0]?.[1] as Record<string, unknown>
}

describe('EditAccountModal 加入号池动态调度', () => {
  beforeEach(() => {
    authIsSimpleMode.value = true
    updateAccountMock.mockReset().mockResolvedValue(account())
  })

  it('reflects the saved enrollment and keeps it (and other credential keys) when untouched', async () => {
    const wrapper = mountModal(account('openai', 'oauth', { ours_tiering: true, ours_home_groups: '8,9,17' }))
    const toggle = wrapper.get('[data-testid="ours-tiering-toggle"]')
    expect(toggle.attributes('aria-checked')).toBe('true')

    const payload = await submit(wrapper)
    const credentials = payload.credentials as Record<string, unknown>
    expect(credentials.ours_tiering).toBe(true)
    expect(credentials.ours_home_groups).toBe('8,9,17')
    expect(credentials.model_mapping).toEqual({ 'gpt-5.6-sol': 'gpt-5.6-sol' })
    wrapper.unmount()
  })

  it('accepts string "1" as enrolled', async () => {
    const wrapper = mountModal(account('openai', 'oauth', { ours_tiering: '1' }))
    expect(wrapper.get('[data-testid="ours-tiering-toggle"]').attributes('aria-checked')).toBe('true')
    wrapper.unmount()
  })

  it('turning it off removes ours_tiering but keeps every other credential key', async () => {
    const wrapper = mountModal(account('openai', 'oauth', { ours_tiering: true, ours_home_groups: '12' }))
    await wrapper.get('[data-testid="ours-tiering-toggle"]').trigger('click')

    const credentials = (await submit(wrapper)).credentials as Record<string, unknown>
    expect(credentials).not.toHaveProperty('ours_tiering')
    expect(credentials.ours_home_groups).toBe('12')
    expect(credentials.model_mapping).toEqual({ 'gpt-5.6-sol': 'gpt-5.6-sol' })
    wrapper.unmount()
  })

  it('turning it on for an API key account writes ours_tiering=true', async () => {
    const wrapper = mountModal({
      ...account('openai', 'apikey', { base_url: 'https://api.example.com' }),
      credentials_status: { has_api_key: true }
    })
    const toggle = wrapper.get('[data-testid="ours-tiering-toggle"]')
    expect(toggle.attributes('aria-checked')).toBe('false')
    await toggle.trigger('click')

    const credentials = (await submit(wrapper)).credentials as Record<string, unknown>
    expect(credentials.ours_tiering).toBe(true)
    wrapper.unmount()
  })

  it('is hidden for non OpenAI-compatible platforms and spark shadow accounts', () => {
    const anthropic = mountModal(account('anthropic', 'oauth'))
    expect(anthropic.find('[data-testid="ours-tiering-section"]').exists()).toBe(false)
    anthropic.unmount()

    const shadow = mountModal({ ...account('openai', 'oauth'), parent_account_id: 7 })
    expect(shadow.find('[data-testid="ours-tiering-section"]').exists()).toBe(false)
    shadow.unmount()
  })
})
