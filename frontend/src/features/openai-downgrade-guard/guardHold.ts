import type { Account } from '@/types'

// 降档哨兵写在账号 extra 里的持有记录（kong_downgrade_guard_hold）。有这条记录表示调度是哨兵关的；
// 人用「可调度」开关打开后，后端随之删掉它。

export type DowngradeGuardStage = 'suspect' | 'confirmed'

export interface DowngradeGuardHold {
  stage: DowngradeGuardStage
  heldAt: string | null
  /** 最后一次确认正常的时刻；没有时影响时段的起点不确定。 */
  lastGoodAt: string | null
}

export function downgradeGuardHold(account: Pick<Account, 'extra'>): DowngradeGuardHold | null {
  const raw = (account.extra as Record<string, unknown> | undefined)?.kong_downgrade_guard_hold
  if (!raw || typeof raw !== 'object') return null
  const hold = raw as Record<string, unknown>
  if (typeof hold.op !== 'string' || hold.op === '') return null
  return {
    stage: hold.stage === 'confirmed' ? 'confirmed' : 'suspect',
    heldAt: typeof hold.held_at === 'string' ? hold.held_at : null,
    lastGoodAt: typeof hold.last_good_at === 'string' ? hold.last_good_at : null,
  }
}

export function guardBadgeText(hold: DowngradeGuardHold): string {
  return hold.stage === 'confirmed' ? '已确认降智' : '疑似降智'
}

/** 本地时间的「月-日 时:分」；取不到时间时返回「未知」。 */
export function formatGuardTime(value: string | null): string {
  if (!value) return '未知'
  const d = new Date(value)
  if (Number.isNaN(d.getTime())) return '未知'
  const pad = (n: number) => String(n).padStart(2, '0')
  return `${pad(d.getMonth() + 1)}-${pad(d.getDate())} ${pad(d.getHours())}:${pad(d.getMinutes())}`
}

export function guardTooltip(hold: DowngradeGuardHold, format: (v: string | null) => string = formatGuardTime): string {
  const reason =
    hold.stage === 'confirmed'
      ? '指纹测试确认这个账号被降到了 luna 档，已停止调度。'
      : '上游不认这个账号自己刚签发的票，已停止调度，正在用指纹测试确认。'
  const from = hold.lastGoodAt ? format(hold.lastGoodAt) : '不确定（按停调前 24 小时算）'
  return [
    reason,
    `停调时间：${format(hold.heldAt)}`,
    `可能受影响的时段：${from} 至 ${format(hold.heldAt)}`,
    '处理完后用「可调度」开关打开。',
  ].join('\n')
}

/**
 * 「可调度」开关操作成功之后，把这些账号本地行里的持有记录去掉：后端在人动开关时删掉它（设成同一个值也删），
 * 不等下次刷新列表。
 */
export function dropDowngradeGuardHold<T extends Pick<Account, 'id' | 'extra'>>(list: T[], ids: Set<number>): T[] {
  return list.map((account) => {
    if (!ids.has(account.id) || !downgradeGuardHold(account)) return account
    const extra = { ...(account.extra as Record<string, unknown>) }
    delete extra.kong_downgrade_guard_hold
    return { ...account, extra }
  })
}
