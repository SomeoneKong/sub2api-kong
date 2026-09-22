// 用量明细里的「请求特征」列（fork 专有）。
//
// 它回答的是"这一条业务请求本身带了什么可用于降智判断的东西"——与票据页面上的统计不同，那边按
// (账号, 模型) 聚合，对不到某一条请求上：一张票服务几十次请求，而 off / observe 模式下客户端自带的
// state 根本不进我们的库。
//
// 抽成纯函数而不是写在模板里：这一列会长出更多特征，而"有哪些、各自怎么显示"是唯一会产生错误陈述
// 的地方（把"没采到"显示成 0、把上一轮的值显示成这一轮的，都属于此），要能单测。

/** 一行特征，界面按 `键: 值` 逐行显示。 */
export interface RequestFeature {
  key: string
  value: string
}

/** 后端下发的特征对象。缺省即"没有这一项"。 */
export interface KongRequestFeatures {
  /** 实际发往上游那个 state 的指纹与长度。 */
  state_fp?: string
  state_len?: number
  /** 客户端自己带来的那一份，只在与出站不同（即被我们替换掉）时才有。 */
  client_state_fp?: string
  client_state_len?: number
  /** 我们注入的那张票（kong_ticket_cache.id）。 */
  ticket_id?: number
  /** 上游在响应里又下发的 state。reissued_ticket_id 只在拿到可确认的库内 id 时才有——缺省涵盖
   *  按规则拒收、落库失败与落库结果未知三种，不能读成"库里没有这张票"。 */
  reissued_fp?: string
  reissued_len?: number
  reissued_ticket_id?: number
}

/** 这一列需要用到的行字段。只声明用到的部分，便于测试构造。 */
export interface RequestFeatureRow {
  kong_request_features?: KongRequestFeatures | null
}

/**
 * 一份 state 的显示值：`长度 (指纹)`。
 *
 * **长度 0 要显示**——它是"带了个空串"，与"没带"是两件事，后者整行不出现。指纹是 sha256 的前 12 个
 * hex 字符：够在一天几千条里区分是不是同一个 state，而原值绝不下发（它是可注入的凭据）。
 */
function stateText(len: number | undefined, fp: string | undefined): string | null {
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

  const outbound = stateText(f.state_len, f.state_fp)
  if (outbound) out.push({ key: 'state', value: outbound })

  const client = stateText(f.client_state_len, f.client_state_fp)
  // 只在我们替换掉客户端那份时才会有它——出现即说明这条请求发生过注入替换。
  if (client) out.push({ key: '客户端 state', value: client })

  if (typeof f.ticket_id === 'number') out.push({ key: '注入票', value: `#${f.ticket_id}` })

  const reissued = stateText(f.reissued_len, f.reissued_fp)
  if (reissued) {
    // 上游回发的票入库了才有 id；312 这类按长度黑名单拒收的只有指纹与长度，那时库里没有这张票。
    const suffix = typeof f.reissued_ticket_id === 'number' ? ` #${f.reissued_ticket_id}` : ''
    out.push({ key: '上游回发', value: reissued + suffix })
  }
  return out
}
