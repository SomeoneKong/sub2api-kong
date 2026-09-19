// Codex 票据管理页的数据类型。设计见仓库根 DESIGN-codex-ticket.md §6。
//
// 这些类型只对应本 fork 自己的 /admin/kong/ticket 端点，不复用上游的账号类型——那个类型
// 上游每月改动多次，依赖它等于把本页面绑在一个高频改动面上。

/** 三种工作模式。off=不参与；observe=只观察不拦截；full=拿不到合格票就拒服。 */
export type TicketMode = 'off' | 'observe' | 'full'

/**
 * 票据出口的三态。**none 与 direct 不是同一件事**：none 表示没有票据出口、不主动取票，
 * direct 表示用服务器本机 IP 取票。
 */
export type TicketEgress = 'none' | 'direct' | 'proxy'

export interface TicketSummary {
  id: number
  model: string
  source: string
  fingerprint_model: string
  fingerprint_p: number
  expires_at: string
  expires_at_source: string
  remaining_seconds: number
}

/** 一次归因观测的摘要。失败与未取样也要能显示，不能被旧的成功结论顶替。 */
export interface TicketDiagnosis {
  at: string
  outcome: string
  /** 为空表示这次没得出归因（拒答、数字不足、候选未被接受等）。 */
  fingerprint_model: string
  probability: number
  ticket_source: string
  reason: string
  verification_id: string
  /** 结论已经比一张票的寿命还老，只反映历史、不代表账号此刻的档位（响应那一刻的快照）。 */
  stale: boolean
  /**
   * 从响应时刻起这个结论还能算「当前」多少秒；已过期为 0。
   *
   * 用剩余秒数而不是绝对时刻：浏览器时钟与服务器可以差好几分钟，拿绝对时刻比会把有效结论
   * 显示成过期（或反之延长它的寿命）。
   */
  stale_after_seconds: number
}

/**
 * 最近一次**取样**的处置。与 diagnosis 回答的问题不同：归因结论只在验证真正跑起来时才有，
 * 而一个账号可能一直在收票、每张都因长度黑名单（312）或间隔未满被挡在验证之前——那时 diagnosis
 * 只会停在旧结论上，看不出「现在根本没在采样」。
 */
export interface TicketSampleNote {
  at: string
  outcome: string
  reason: string
  state_len: number | null
}

/** 一个账号在某一个门控模型上的状态。票与结论都是 (account, model) 绑定的。 */
export interface TicketModelStatus {
  model: string
  current_ticket: TicketSummary | null
  unverified_count: number
  /**
   * 最近一次归因结论。它与 current_ticket 是两件事：一张旧的 verified astra 票可以还在服务，
   * 而最近一次探测已经归因为 sol——那正是「该给这个账号开 full 了」的信号。
   */
  diagnosis: TicketDiagnosis | null
  /** 最近一次取样处置，解释「现在在采什么样」。 */
  last_sample: TicketSampleNote | null
}

export interface TicketAccountStatus {
  account_id: number
  name: string
  platform: string
  /** 账号此刻可正常调度。为假时诊断会暂停，原因见 not_ready。 */
  ready: boolean
  not_ready: string

  mode: TicketMode
  egress: TicketEgress
  proxy_id: number | null
  /** 被拒绝的配置键。非空说明存着一份读不出来的配置，要在界面上显式提示。 */
  config_rejected: string[] | null

  traffic_egress: string
  ticket_egress: string
  egress_usable: boolean
  egress_reason: string

  /** 按门控模型逐个给出。为空表示功能未启用。 */
  models: TicketModelStatus[] | null

  /**
   * 距**本系统**最后一次使用该票据出口的秒数，不是实际静默——系统外的活动观测不到，
   * 这个值会高估真实静默。界面上必须带这个限定，不能显示成「现在一定能取到好票」。
   */
  egress_idle_seconds: number | null
  /** 下一次主动取票最早被允许的时刻。 */
  next_fetch_allowed_at: string | null
}

export interface TicketParams {
  refresh_before_seconds: number
  ticket_fetch_min_idle_seconds: number
  verify_fail_cooldown_seconds: number
  min_ticket_age_seconds: number
  observe_probe_interval_seconds: number
}

export interface TicketOverview {
  accounts: TicketAccountStatus[] | null
  /** 功能是否生效（门控模型集合非空）。为假时配置不可写，页面要显示「未启用」而不是故障。 */
  enabled: boolean
  /** 门控模型集合。为空即整个功能不生效，界面要显式说明这一点。 */
  gated_models: string[] | null
  params: TicketParams
  idle_seconds_caveat: string
}

export interface TicketEvent {
  id: number
  created_at: string
  account_id: number
  model: string
  event_type: string
  outcome: string
  status_code: number | null
  traffic_egress: string
  ticket_egress: string
  state_len: number | null
  ticket_id: number | null
  fingerprint_model: string | null
  idle_seconds: number | null
  detail: Record<string, unknown> | null
}

export interface TicketEventPage {
  items: TicketEvent[] | null
  total: number
  limit: number
  offset: number
}

export interface TicketEventQuery {
  account_id?: number
  /** 按最终上游模型过滤。诊断与票都是 (account, model) 绑定的。 */
  model?: string
  event_type?: string
  limit?: number
  offset?: number
}

export interface TicketConfigRequest {
  mode: TicketMode
  egress: TicketEgress
  /** proxy 以外的出口一律传 null；不沿用上游「提交 0 表示清除」的约定。 */
  proxy_id: number | null
}

/** 一份指纹探测记录。原始数字序列不在接口里返回——几百个数字在页面上看不出任何东西。 */
export interface FingerprintProbe {
  id: number
  created_at: string
  part_index: number
  account_id: number
  target_model: string
  ticket_source: string
  verify_egress: string
  challenge_id: string
  digit_count: number
  /** 这一份自己最像哪个模型；与 cum_probability 的累计结论不是一回事。 */
  part_attribution: string | null
  cum_probability: number | null
  temperature_tier: number | null
  library_version: Record<string, unknown> | null
  parse_valid: boolean
  counted_in_average: boolean
  invalid_reason: string | null
  latency_ms: number | null
  output_tokens: number | null
}
