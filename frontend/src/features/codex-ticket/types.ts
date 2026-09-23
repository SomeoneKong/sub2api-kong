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
  /**
   * 这个结论出自哪一层：0 = stg0（上游自己回报的 model），1 或 null = stg1（指纹归因）。
   *
   * 显示时必须据它区分来源，**不能按「概率是不是 1」去猜**：stg0 没有概率，上游声明与一次恰好很
   * 确定的归因是两种可信度完全不同的证据。
   */
  stg: number | null
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
  /**
   * 近三天业务请求上的 stg0 观测（上游回报的 model 是否就是请求的那个）。
   *
   * 为 null 表示该窗口内没有样本、或统计查询失败——两者都要显示成「无样本」而不是 0%，后者会被
   * 读成「查过了、没问题」。
   */
  stg0: TicketStg0Stats | null
  /** 最近一次取样处置，解释「现在在采什么样」。 */
  last_sample: TicketSampleNote | null
}

/**
 * stg0 的近期观测汇总。三个计数互斥、相加等于窗口内该模型的受控请求数。
 *
 * `unknown` 必须单独看：它是「没拿到上游回报值」，与「一致」是两件事。全是 unknown 时
 * 「零次不一致」是假的安全感——那说明观测没工作，不是没被降智。
 */
export interface TicketStg0Stats {
  account_id: number
  model: string
  total: number
  mismatch: number
  unknown: number
  /**
   * `mismatch` 按 stg0 白名单拆成的两份，相加恒等于它。
   *
   * 两份的处置完全不同：`mismatch_accepted` 是上游在投放我们认可的替代模型（验票不会判死票，
   * 无须动作），`mismatch_unaccepted` 才是「上游自己声明给了别的模型且我们没放行」。**标红只看后者**
   * ——配好白名单之后仍然长期标红，会让这块读数永久失去可操作性。
   */
  mismatch_accepted: number
  mismatch_unaccepted: number
  /**
   * 不一致时上游回报过的 model 及次数（最多几项）。未接受的排在前面——它们是唯一要人动手的那类。
   *
   * 它是这块信息里最可操作的部分：上游投放新模型时，运维照着它往 stg0_accept 加一条即可。只给
   * 比例的话，看到「mismatch 87%」也不知道该加什么。
   *
   * **声明成可能为 null**：Go 的 nil 切片序列化成 `null`，历史数据与明细查询失败的分支都可能给
   * 这个值。按数组直接读 `.length` 会打崩整个页面，所以一律经归一化再用。
   */
  top_reported: Array<{ model: string; count: number; accepted: boolean }> | null
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
  /**
   * 两张白名单当前生效的内容（`模型 → 接受值`），同属生效范围：它们决定「上游给了别的东西时算不算
   * 合格」。`stg0_accept` 收的是上游回报的 model 名，`fingerprint_accept` 补偿的是指纹归因的区分度
   * 不足——**两者互抄值会放过真的降智**，所以页面也分两行显示。
   */
  stg0_accept: Record<string, string[]> | null
  fingerprint_accept: Record<string, string[]> | null
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

// 三个条件都是多值：组内 OR、组间 AND，空数组即该维度不过滤。
export interface TicketEventQuery {
  account_ids?: number[]
  /** 按最终上游模型过滤。诊断与票都是 (account, model) 绑定的。 */
  models?: string[]
  event_types?: string[]
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
  /** 这一份来自取票请求本身（融合取票）：产生于票据出口、当时没有注入票。 */
  fused: boolean
  /**
   * 非空表示这一份**留档但不采用**：回答可能有效，只是产生条件不同、不参与最终归因。与
   * invalid_reason（回答本身无效）分开；它不代表票被判死。空串表示采用。
   */
  discarded_reason: string
  latency_ms: number | null
  output_tokens: number | null
}

/** 一次手工触发的结论。**不含票原值**——那是可注入的凭据，服务端刻意不下发。 */
export interface TicketRefreshResult {
  /** 非零表示本次先把一张未过期的已拒票复位成候选，走的是重验而不是取新票。 */
  revived_ticket_id: number
  allowed: boolean
  ticket_id: number
  /** 未拿到票的原因；多数不是故障（静默未满、模式不是 full 都是正常结论）。 */
  deny_reason: string
  /** 该账号不在保护范围内（mode 不是 full），与"该保护但保不了"是两件事。 */
  not_applicable: boolean
  retry_after: string | null
}

// 票据详情页上的一行。**不含票原值**——那是可注入的凭据，服务端只给派生信息。
export interface TicketDetailRow {
  id: number
  model: string
  status: string
  source: string
  state_len: number
  /** 已被排除出候选池（注入未被上游接受，或本段无票期机会用完）。 */
  skip_until_new: boolean
  captured_at: string
  expires_at: string
  expires_at_source: string
  remaining_seconds: number
  fingerprint_model: string
  fingerprint_p: number
  /** 各模型的归因概率，用来解释"为什么这张判不合格"。 */
  fingerprint_probs: Record<string, number> | null
  /**
   * 这个结论出自哪一层：0 = stg0（上游自己回报的 model），1 = stg1（指纹归因），null = 未知（历史数据）。
   *
   * stg0 判死票时会把回报值当归因结果写进票行（p=1、单点分布），那是为了让下游统一按概率工作。
   * 但**显示时必须据这个字段区分来源**：照 p 显示就把上游的一句声明呈现成"指纹归因，置信度 1.00"。
   * 不能按"概率是不是 1"去猜——stg1 的概率也可以恰好是 1。
   */
  stg: number | null
  /**
   * 此刻业务注入的就是它：按当前白名单与阈值是首选票，**且这个账号真的会注入**（mode=full）。
   * 由服务端算，前端不能按 status 自己猜。
   */
  is_current: boolean
  /** 按当前判据它是该模型的首选票。与 is_current 的差别只在模式——off / observe 并不注入。 */
  preferred: boolean
  /** 现在可以手工验。已拒的与被跳过的都可以（服务端会先准备）；已过期、账号不可调度的不行。 */
  verifiable: boolean
  /** 为什么不能验。空串表示可以验——「已过期」与「账号不可调度」是不同的两件事，原因由服务端给。 */
  not_verifiable_reason: string
}

// 某个账号的票据详情。
export interface TicketDetailPage {
  account: TicketAccountStatus
  tickets: TicketDetailRow[] | null
  /** 还有更早的票没列出来（撞到行数上限）。 */
  truncated: boolean
}

export interface TicketVerifyTicketResponse {
  result: TicketVerifyResult
  detail: TicketDetailPage | null
}

// 立即验票序列里的一张。
export interface TicketManualVerifyStep {
  ticket_id: number
  candidate: boolean
  accepted: boolean
  revoked: boolean
  inconclusive: boolean
  reason: string
}

// 立即验票的结果。与取票分开：那个可能取新票，这个只验现有的票（当前票 + 最多一张候选）。
export interface TicketVerifyResult {
  /** 最后一步验的那张票；一张都没验时为 0。 */
  ticket_id: number
  /** 重新自证合格，仍可用。 */
  accepted: boolean
  /** 已把它作废：证据完整但归因不合格，或上游明确重发了票。 */
  revoked: boolean
  /** 没能完成测量（超时、429、前提失效、写库失败）；旧票与旧结论保留。与 revoked 互斥。 */
  inconclusive: boolean
  /** 验的是一张候选，不是正在服务的票——失败后果不同，文案要分开。 */
  candidate: boolean
  /** 还有一张候选没验：同步端点余量不够再跑一张，提示可以再点一次。 */
  budget_exhausted: boolean
  /** 本次实际验过的每一张，按执行顺序；顶层字段等于最后一步。 */
  steps: TicketManualVerifyStep[]
  reason: string
  /** 压根没票可验（当前票与可验候选都没有）。模式不是它的成因——三种模式都能验。 */
  not_applicable: boolean
  /** 没能开始验的原因（同账号有任务在途之类）。 */
  deny_reason: string
}

export interface TicketVerifyResponse {
  result: TicketVerifyResult | null
  status: TicketAccountStatus | null
}

export interface TicketRefreshResponse {
  result: TicketRefreshResult | null
  /** 触发后该行的最新状态；服务端一并返回，避免调用方重拉 overview 冲掉别行的草稿。 */
  status: TicketAccountStatus | null
}
