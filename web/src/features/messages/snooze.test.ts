import {describe, expect, it} from 'vitest'
import {snoozeTimestamp} from './snooze'
import {batchResponse, messageResponse} from './responses'

describe('local reminder dates', () => {
  const monday = new Date(2026, 8, 14, 16, 12, 30)
  it('uses elapsed time for one hour and calendar dates for morning presets', () => {
    expect(snoozeTimestamp('hour', '', monday)).toBe(Math.floor(monday.getTime() / 1000) + 3600)
    expect(new Date(snoozeTimestamp('1', '', monday) * 1000)).toEqual(new Date(2026, 8, 15, 9))
    expect(new Date(snoozeTimestamp('3', '', monday) * 1000)).toEqual(new Date(2026, 8, 17, 9))
    expect(new Date(snoozeTimestamp('7', '', monday) * 1000)).toEqual(new Date(2026, 8, 21, 9))
    expect(new Date(snoozeTimestamp('monday', '', monday) * 1000)).toEqual(new Date(2026, 8, 21, 9))
    expect(new Date(snoozeTimestamp('monday', '', new Date(2026, 8, 20, 16)) * 1000)).toEqual(new Date(2026, 8, 21, 9))
    expect(new Date(snoozeTimestamp('1', '', new Date(2026, 11, 31, 23)) * 1000)).toEqual(new Date(2027, 0, 1, 9))
  })
  it('interprets custom datetime as local time and rejects impossible or past input', () => {
    expect(new Date(snoozeTimestamp('custom', '2026-09-15T10:30', monday) * 1000)).toEqual(new Date(2026, 8, 15, 10, 30))
    for (const value of ['', 'invalid', '2026-09-14T16:12', '2026-01-01T09:00', '2027-02-30T09:00', '2026-13-01T09:00', '2026-09-15T24:00', '2026-09-15T10:60', '2026-09-15T10:30Z']) {
      expect(() => snoozeTimestamp('custom', value, monday)).toThrow()
    }
    expect(() => snoozeTimestamp('hour', '', new Date(NaN))).toThrow()
  })
  it('keeps calendar mornings across a DST transition and refuses skipped local times', () => {
    const before = new Date(2027, 2, 13, 12)
    expect(new Date(snoozeTimestamp('1', '', before) * 1000)).toEqual(new Date(2027, 2, 14, 9))
    const candidate = new Date(2027, 2, 14, 2, 30)
    if (candidate.getHours() !== 2) expect(() => snoozeTimestamp('custom', '2027-03-14T02:30', before)).toThrow('존재하지 않는')
    else expect(snoozeTimestamp('custom', '2027-03-14T02:30', before)).toBe(candidate.getTime() / 1000)
  })
  it('retains a valid server reminder timestamp and refuses unusable date values', () => {
    const message = {id: 'm1', account_id: 'own', from: {email: 'a@corp.local'}}
    expect(messageResponse({...message, snoozed_until: 1800000000}).snoozed_until).toBe(1800000000)
    expect(messageResponse(message).snoozed_until).toBeUndefined()
    for (const value of [-1, 0.5, Infinity, Number.MAX_VALUE, 'tomorrow']) expect(() => messageResponse({...message, snoozed_until: value})).toThrow()
  })
  it('only accepts complete, consistent batch results for the submitted messages', () => {
    expect(batchResponse({succeeded: 1, failed: 1, results: [{message_id: 'm1', ok: true}, {message_id: 'm2', ok: false}]}, ['m1', 'm2']).failed).toBe(1)
    for (const payload of [
      {succeeded: 1, failed: 0, results: [{message_id: 'different-user-message', ok: true}]},
      {succeeded: 1, failed: 0, results: [{message_id: 'm1', ok: false}]},
      {succeeded: 2, failed: 0, results: [{message_id: 'm1', ok: true}, {message_id: 'm1', ok: true}]},
      {succeeded: 0, failed: 0, results: []},
    ]) expect(() => batchResponse(payload, ['m1'])).toThrow()
  })
})
