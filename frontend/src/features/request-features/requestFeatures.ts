// 用量明细里的「请求特征」列（fork 专有）：这一轮发往上游的 codex 票与上游在响应里下发的票，
// 各自的长度与指纹。
//
// 抽成纯函数而不是写在模板里：缺值怎么显示是唯一会产生错误陈述的地方（把"没采到"显示成 0 即属此类），
// 要能单测。

/** 一行特征，界面按 `键: 值` 逐行显示。 */
export interface RequestFeature {
  key: string
  value: string
}

/** 后端下发的特征对象。每一项都可能缺省，缺省即"没有这一项"。 */
export interface KongRequestFeatures {
  /** 这一轮实际发往上游的票的长度与指纹。 */
  state_len?: number
  state_fp?: string
  /** 上游在这一轮响应里下发的票的长度与指纹；一轮下发多张时是最后一张。 */
  reissued_len?: number
  reissued_fp?: string
}

/** 这一列需要用到的行字段。只声明用到的部分，便于测试构造。 */
export interface RequestFeatureRow {
  kong_request_features?: KongRequestFeatures | null
}

/**
 * 一张票的显示值：`长度 (指纹)`，缺哪项就省哪项。
 *
 * **长度 0 要显示**——它是"带了个空串"，与"没带"是两件事，后者整行不出现。指纹是 sha256 的前 12 个
 * hex 字符，票的原值不下发。
 */
function ticketText(len: number | undefined, fp: string | undefined): string | null {
  if (typeof len !== 'number' && !fp) return null
  const parts: string[] = []
  if (typeof len === 'number') parts.push(String(len))
  if (fp) parts.push(`(${fp})`)
  return parts.join(' ')
}

/** 逐行列出该请求的特征。没有任何特征时返回空数组，由界面显示占位符。 */
export function requestFeatures(row: RequestFeatureRow | null | undefined): RequestFeature[] {
  const f = row?.kong_request_features
  if (!f) return []
  const out: RequestFeature[] = []

  const sent = ticketText(f.state_len, f.state_fp)
  if (sent) out.push({ key: 'state', value: sent })

  const reissued = ticketText(f.reissued_len, f.reissued_fp)
  if (reissued) out.push({ key: '上游回发', value: reissued })

  return out
}
