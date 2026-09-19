import { apiClient } from '@/api/client'
import type {
  FingerprintProbe,
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
  if (query.account_id !== undefined) params.account_id = query.account_id
  if (query.model) params.model = query.model
  if (query.event_type) params.event_type = query.event_type
  const { data } = await apiClient.get<TicketEventPage>(`${basePath}/events`, { params })
  return data
}

export async function listProbes(verificationID: string): Promise<FingerprintProbe[]> {
  const { data } = await apiClient.get<{ items: FingerprintProbe[] | null }>(
    `${basePath}/probes/${encodeURIComponent(verificationID)}`,
  )
  return data.items ?? []
}

export default { getOverview, updateAccountConfig, listEvents, listProbes }
