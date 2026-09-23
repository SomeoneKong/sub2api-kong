import { describe, expect, it } from 'vitest'

import { requestFeatures } from '../requestFeatures'

// 这一列唯一会产生错误陈述的地方是缺值与半缺值：把"没采到"显示成 0、或给一张没入库的票编个 id，
// 都会让排查照着不存在的事实走。所以这里钉的是"缺什么就不显示什么"。
describe('requestFeatures', () => {
  it('出站 state 按「长度 (指纹)」显示', () => {
    expect(requestFeatures({ kong_request_features: { state_len: 292, state_fp: 'a1b2c3d4e5f6' } })).toEqual([
      { key: 'state', value: '292 (a1b2c3d4e5f6)' },
    ])
  })

  it('替换过客户端那份时，两份都列出来', () => {
    const lines = requestFeatures({
      kong_request_features: {
        state_len: 292,
        state_fp: 'aaaaaaaaaaaa',
        client_state_len: 312,
        client_state_fp: 'bbbbbbbbbbbb',
        ticket_id: 7,
      },
    })
    expect(lines.map((l) => l.key)).toEqual(['state', '客户端 state', '注入票'])
    expect(lines[1].value).toBe('312 (bbbbbbbbbbbb)')
    expect(lines[2].value).toBe('#7')
  })

  it('上游回发的票入库了就带 id，没入库只有长度与指纹', () => {
    const stored = requestFeatures({
      kong_request_features: { reissued_len: 292, reissued_fp: 'cccccccccccc', reissued_ticket_id: 91 },
    })
    expect(stored).toEqual([{ key: '上游回发', value: '292 (cccccccccccc) #91' }])

    // 312 在长度黑名单里，不入库——这时库里根本没有这张票，不能编一个 id 出来。
    const rejected = requestFeatures({
      kong_request_features: { reissued_len: 312, reissued_fp: 'dddddddddddd' },
    })
    expect(rejected).toEqual([{ key: '上游回发', value: '312 (dddddddddddd)' }])
  })

  it('长度 0 要显示——那是「带了个空串」，与「没带」不同', () => {
    expect(requestFeatures({ kong_request_features: { state_len: 0 } })).toEqual([
      { key: 'state', value: '0' },
    ])
  })

  it('没采到特征时整列为空，不退化成 0', () => {
    expect(requestFeatures({ kong_request_features: null })).toEqual([])
    expect(requestFeatures({})).toEqual([])
    expect(requestFeatures(null)).toEqual([])
    expect(requestFeatures(undefined)).toEqual([])
    expect(requestFeatures({ kong_request_features: {} })).toEqual([])
  })
})
