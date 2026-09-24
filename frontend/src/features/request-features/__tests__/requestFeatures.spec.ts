import { describe, expect, it } from 'vitest'

import { requestFeatures, type RequestFeatureRow } from '../requestFeatures'

// 这一列唯一会产生错误陈述的地方是缺值与半缺值：把"没采到"显示成 0 会让排查照着不存在的事实走。
// 所以这里钉的是"缺什么就不显示什么"。
describe('requestFeatures', () => {
  it('两项都有时按 state、上游回发的顺序逐行显示「长度 (指纹)」', () => {
    expect(
      requestFeatures({
        kong_request_features: {
          state_len: 292,
          state_fp: 'a1b2c3d4e5f6',
          reissued_len: 312,
          reissued_fp: 'cccccccccccc',
        },
      }),
    ).toEqual([
      { key: 'state', value: '292 (a1b2c3d4e5f6)' },
      { key: '上游回发', value: '312 (cccccccccccc)' },
    ])
  })

  it('只有一项时只显示那一项', () => {
    expect(requestFeatures({ kong_request_features: { state_len: 292, state_fp: 'aaaaaaaaaaaa' } })).toEqual([
      { key: 'state', value: '292 (aaaaaaaaaaaa)' },
    ])
    expect(requestFeatures({ kong_request_features: { reissued_len: 292, reissued_fp: 'bbbbbbbbbbbb' } })).toEqual([
      { key: '上游回发', value: '292 (bbbbbbbbbbbb)' },
    ])
  })

  it('长度 0 要显示——那是「带了个空串」，与「没带」不同', () => {
    expect(requestFeatures({ kong_request_features: { state_len: 0 } })).toEqual([{ key: 'state', value: '0' }])
    expect(requestFeatures({ kong_request_features: { reissued_len: 0, reissued_fp: 'dddddddddddd' } })).toEqual([
      { key: '上游回发', value: '0 (dddddddddddd)' },
    ])
  })

  it('只有指纹没有长度时只显示指纹，不补一个 0', () => {
    expect(requestFeatures({ kong_request_features: { state_fp: 'eeeeeeeeeeee' } })).toEqual([
      { key: 'state', value: '(eeeeeeeeeeee)' },
    ])
  })

  it('全缺省时整列为空，不退化成 0', () => {
    expect(requestFeatures({ kong_request_features: null })).toEqual([])
    expect(requestFeatures({})).toEqual([])
    expect(requestFeatures(null)).toEqual([])
    expect(requestFeatures(undefined)).toEqual([])
    expect(requestFeatures({ kong_request_features: {} })).toEqual([])
  })

  it('旧行里残留的 client_state_* / ticket_id 等键被忽略', () => {
    const legacy = {
      kong_request_features: {
        state_len: 292,
        state_fp: 'aaaaaaaaaaaa',
        client_state_len: 312,
        client_state_fp: 'bbbbbbbbbbbb',
        ticket_id: 7,
        reissued_ticket_id: 91,
      },
    } as unknown as RequestFeatureRow
    expect(requestFeatures(legacy)).toEqual([{ key: 'state', value: '292 (aaaaaaaaaaaa)' }])

    const onlyLegacy = {
      kong_request_features: { client_state_len: 312, ticket_id: 7, reissued_ticket_id: 91 },
    } as unknown as RequestFeatureRow
    expect(requestFeatures(onlyLegacy)).toEqual([])
  })
})
