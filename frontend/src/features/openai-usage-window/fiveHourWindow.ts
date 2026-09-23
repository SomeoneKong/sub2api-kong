import { normalizePlanType } from '@/utils/planType'

/**
 * OpenAI Pro 20x / Pro 5x 只有一个 7d 主窗口，5h 窗口恒未激活，用量窗口列里的 5h 进度条
 * 对它们只是噪音。套餐按账号页的口径取：自身 credentials.plan_type 优先，空时用父账号的。
 */
const PLANS_WITHOUT_FIVE_HOUR_WINDOW = new Set(['pro', 'chatgptpro', 'prolite'])

interface PlanTypeSource {
  credentials?: Record<string, unknown> | null
  parent_plan_type?: string | null
}

export function hidesOpenAIFiveHourWindow(account: PlanTypeSource): boolean {
  const own = account.credentials?.plan_type
  const planType = typeof own === 'string' && own.trim() ? own : account.parent_plan_type
  return PLANS_WITHOUT_FIVE_HOUR_WINDOW.has(normalizePlanType(planType))
}
