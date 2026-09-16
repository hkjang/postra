import {describe, expect, it} from 'vitest'
import {canonicalWorkStatus, filterAndSortWork, workDueBucket, workDueCounts, type TeamItem} from './deadlines'

const row = (id: string, due?: number, status = 'new', updated = 1): TeamItem => ({collab: {message_id: id, sla_due: due, status, updated_at: updated}})
const unix = (date: Date) => date.getTime() / 1000

describe('local workflow deadlines', () => {
  const now = new Date(2026, 8, 16, 12, 30)
  it.each(['__proto__', 'constructor', 'toString', 'hasOwnProperty', 'custom_status'])('preserves unknown or prototype-like status %s as a string', status => {
    expect(canonicalWorkStatus(status)).toBe(status)
  })
  it('uses mutually exclusive current-time and local-midnight boundaries', () => {
    expect(workDueBucket(row('past', unix(now) - 1), now)).toBe('overdue')
    expect(workDueBucket(row('now', unix(now)), now)).toBe('today')
    expect(workDueBucket(row('end', unix(new Date(2026, 8, 16, 23, 59, 59))), now)).toBe('today')
    expect(workDueBucket(row('next', unix(new Date(2026, 8, 17))), now)).toBe('upcoming')
    const midnight = new Date(2026, 8, 17)
    expect(workDueBucket(row('last-day', unix(midnight) - 1), midnight)).toBe('overdue')
    expect(workDueBucket(row('first-second', unix(midnight)), midnight)).toBe('today')
  })

  it('uses next local calendar day rather than a fixed 24-hour interval around DST changes', () => {
    for (const [month, day] of [[2, 8], [10, 1]]) {
      const midnight = new Date(2026, month, day)
      const nextMidnight = new Date(2026, month, day + 1)
      expect(workDueBucket(row('last-local-second', unix(nextMidnight) - 1), midnight)).toBe('today')
      expect(workDueBucket(row('next-local-day', unix(nextMidnight)), midnight)).toBe('upcoming')
    }
  })

  it.each([undefined, 0, -1, NaN, Infinity, 9e15])('treats absent or invalid deadline %s as no due', value => {
    expect(workDueBucket(row('no-date', value), now)).toBe('none')
  })

  it('keeps completed legacy and current states out of active deadline counts', () => {
    const values = [row('overdue', unix(now) - 1), row('today', unix(now)), row('upcoming', unix(new Date(2026, 8, 17))), row('no-date'), row('done', unix(now) - 1000, 'done'), row('resolved', undefined, 'resolved')]
    expect(workDueCounts(values, now)).toEqual({all: 6, overdue: 1, today: 1, upcoming: 1, none: 1, completed: 2})
    expect(filterAndSortWork(values, 'none', 'recent', now).map(item => item.collab.message_id)).toEqual(['no-date'])
  })

  it('sorts active deadlines first, missing dates last, and completed work by recent update without mutating input', () => {
    const values = [row('no-date', undefined, 'new', 8), row('future', unix(now) + 100, 'new', 9), row('done-older', 1, 'done', 1), row('due-first', unix(now) - 100, 'pending', 3), row('done-newer', 2, 'resolved', 10)]
    const original = [...values]
    expect(filterAndSortWork(values, 'all', 'deadline', now).map(item => item.collab.message_id)).toEqual(['due-first', 'future', 'no-date', 'done-newer', 'done-older'])
    expect(filterAndSortWork(values, 'all', 'recent', now).map(item => item.collab.message_id)).toEqual(['done-newer', 'future', 'no-date', 'due-first', 'done-older'])
    expect(values).toEqual(original)
    expect(filterAndSortWork([row('b', 1), row('a', 1)], 'all', 'deadline', now).map(item => item.collab.message_id)).toEqual(['a', 'b'])
  })
})
