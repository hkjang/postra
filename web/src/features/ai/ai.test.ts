import {describe, it, expect} from 'vitest'
import {resultOf} from './index'
describe('AI result runtime validation', () => {
  it('keeps valid answers and grounded evidence', () => {
    expect(resultOf({id: 'an', model: 'fake', result_json: '{"answer":"검토가 필요합니다","evidence_message_ids":["msg_1"]}'})).toMatchObject({answer: '검토가 필요합니다', evidence_message_ids: ['msg_1']})
  })
  it('does not render malformed model arrays and objects as React children', () => {
    const result = resultOf({id: 'an', model: 'fake', result_json: '{"answer":{"unsafe":"shape"},"fyi":{},"needs_reply":"not an array","deadlines":[null],"confidence":999}'})
    expect(result?.answer).toBeUndefined()
    expect(result?.fyi).toBeUndefined()
    expect(result?.needs_reply).toBeUndefined()
    expect(result?.deadlines).toBeUndefined()
    expect(result?.confidence).toBeUndefined()
  })
})
