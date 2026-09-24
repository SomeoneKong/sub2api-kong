import { shallowRef } from 'vue'
import type { Account } from '@/types'

/**
 * 正在做指纹测试的账号；null 表示弹窗关闭。
 *
 * 放在模块级而不是菜单组件里：入口在账号行菜单里，菜单关闭时会随之卸载，弹窗由账号页挂着的
 * FingerprintTestHost 按这个状态显示。
 */
export const fingerprintTestAccount = shallowRef<Account | null>(null)

/**
 * 只有走 codex 协议的 OpenAI 账号（oauth / setup-token）能做指纹测试。两类除外（后端同样拒绝）：
 * 影子账号没有自己的凭据，测它的母账号即可；Agent Identity 账号不持有 access token，每个请求都要
 * 现场签名，不在测试范围内。
 */
export function isFingerprintTestable(
  account: Pick<Account, 'platform' | 'type' | 'parent_account_id' | 'credentials'> | null | undefined
): boolean {
  if (account?.platform !== 'openai' || (account.type !== 'oauth' && account.type !== 'setup-token')) {
    return false
  }
  const authMode = account.credentials?.auth_mode
  const agentIdentity = typeof authMode === 'string' && authMode.trim().toLowerCase() === 'agentidentity'
  return account.parent_account_id == null && !agentIdentity
}

export function openFingerprintTest(account: Account): void {
  fingerprintTestAccount.value = account
}

export function closeFingerprintTest(): void {
  fingerprintTestAccount.value = null
}
