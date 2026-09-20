import { apiClient } from '@/api/client'
import type {
  FingerprintProbe,
  TicketRefreshResponse,
  TicketVerifyResponse,
  TicketAccountStatus,
  TicketConfigRequest,
  TicketEventPage,
  TicketEventQuery,
  TicketOverview,
} from './types'

// 本页面的四个端点。路由注册在 backend/internal/server/routes/admin_kong_ticket.go。

const basePath = '/admin/kong/ticket'

export async function getOverview(): Promise<TicketOverview> {
  const { data } = await apiClient.get<TicketOverview>(`${basePath}/overview`)
  return data
}

export async function updateAccountConfig(
  accountID: number,
  payload: TicketConfigRequest,
): Promise<TicketAccountStatus> {
  const { data } = await apiClient.put<TicketAccountStatus>(`${basePath}/accounts/${accountID}`, payload)
  return data
}

export async function listEvents(query: TicketEventQuery = {}): Promise<TicketEventPage> {
  const params: Record<string, string | number> = {
    limit: query.limit ?? 50,
    offset: query.offset ?? 0,
  }
  // 多值条件用逗号拼一个参数，不用 axios 默认的 `key[]=` 数组序列化——后端读的是裸参数名。
  if (query.account_ids?.length) params.account_id = query.account_ids.join(',')
  if (query.models?.length) params.model = query.models.join(',')
  if (query.event_types?.length) params.event_type = query.event_types.join(',')
  const { data } = await apiClient.get<TicketEventPage>(`${basePath}/events`, { params })
  return data
}

export async function listProbes(verificationID: string): Promise<FingerprintProbe[]> {
  const { data } = await apiClient.get<{ items: FingerprintProbe[] | null }>(
    `${basePath}/probes/${encodeURIComponent(verificationID)}`,
  )
  return data.items ?? []
}

// triggerRefresh 手工触发一次取票/验票。
//
// 同步等结果：整条路径最坏是取票 + 三份挑战，调用方要据此显示结论。拿不到票**不是**错误
// （静默未满等都是正常结论），所以只有 HTTP 层失败才会 reject。
export async function triggerRefresh(
  accountID: number,
  model: string,
): Promise<TicketRefreshResponse> {
  const { data } = await apiClient.post<TicketRefreshResponse>(
    `${basePath}/accounts/${accountID}/refresh`,
    { model },
    // 必须覆盖 apiClient 的 30s 默认超时：服务端的等待上限是 5 分钟（kongWaitBudget），一次取票
    // 加三份挑战完全可能超过 30 秒。超时后后台任务仍会跑完（它脱离请求 context），但调用方拿不到
    // 结论，同步返回的设计就白费了。留半分钟余量。
    { timeout: 330_000 },
  )
  return data
}

// triggerVerify 立即重验当前票。**真验出问题时服务端会把它作废**（证据完整而归因不合格，或上游
// 明确重发了票），所以这是一次有副作用的调用；探测失败只报「未得出结论」，旧票保留。
//
// 330s 是刻意比服务端大一档：那边的同步验证上限是 kongWaitBudget(300s)，客户端留 30 秒余量，
// 于是调用方总能拿到真实结论——不会出现"页面报失败、后台还在改票状态"。
export async function triggerVerify(
  accountID: number,
  model: string,
): Promise<TicketVerifyResponse> {
  const { data } = await apiClient.post<TicketVerifyResponse>(
    `${basePath}/accounts/${accountID}/verify`,
    { model },
    { timeout: 330_000 },
  )
  return data
}

export default {
  getOverview,
  updateAccountConfig,
  listEvents,
  listProbes,
  triggerRefresh,
  triggerVerify,
}
