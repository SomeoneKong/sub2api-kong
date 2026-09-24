// 账号指纹测试的数据类型，对应 fork 自己的端点：
//   GET  /admin/kong/fingerprint/targets
//   POST /admin/accounts/:id/kong-fingerprint-test（SSE）

/** 一个可选的目标模型。 */
export interface FingerprintTarget {
  model: string
  display_name: string
}

/** 一个候选模型与它的概率。 */
export interface FingerprintCandidate {
  model: string
  display_name: string
  probability: number
}

/** 一份挑战的结果。part_started 事件里只有 index 与 challenge_id。 */
export interface FingerprintPart {
  index: number
  challenge_id: string
  status_code?: number
  error?: string
  /** 上游在这一份响应里回报的模型；缺省表示没观测到。 */
  reported_model?: string
  digit_count?: number
  /** 这一份计入了归因。 */
  valid?: boolean
  invalid_reason?: string
  /** 这一份**自己**最像的模型；cumulative 是累计到这一份为止的分布（前几名）。 */
  attribution?: string
  cumulative?: FingerprintCandidate[]
  latency_ms?: number
  output_tokens?: number
}

/** 执行结果：这次测试有没有跑完。 */
export type FingerprintExecution = 'completed' | 'cancelled' | 'failed'

/** 模型结论：跑出来的证据指向什么。只在执行完成时才有。 */
export type FingerprintVerdict = 'match' | 'mismatch' | 'inconclusive'

/** 整次测试的结论。 */
export interface FingerprintResult {
  execution: FingerprintExecution
  end_reason: string
  detail?: string
  verdict?: FingerprintVerdict
  parts: number
  candidates?: FingerprintCandidate[]
  /** 非空表示有证据没写进库：结论照常给出，但事后读库还原不全。 */
  persist_error?: string
}

export type FingerprintTestEvent =
  | { type: 'started'; test_id: string; target_model: string; max_parts: number }
  | { type: 'part_started'; test_id: string; part: FingerprintPart }
  | { type: 'part'; test_id: string; part: FingerprintPart }
  | { type: 'done'; test_id: string; result: FingerprintResult }
