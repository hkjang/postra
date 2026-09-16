import type {Message} from '@/features/messages/types'

export type Collab = {message_id: string; assignee?: string; status: string; sla_due?: number; updated_at: number}
export type TeamItem = {collab: Collab; message?: Message}
export type DueFilter = 'all' | 'overdue' | 'today' | 'upcoming' | 'none'
export type WorkOrder = 'recent' | 'deadline'
export const dueFilters: {id: DueFilter; label: string}[] = [{id: 'all', label: '전체'}, {id: 'overdue', label: '지연'}, {id: 'today', label: '오늘'}, {id: 'upcoming', label: '예정'}, {id: 'none', label: '기한 없음'}]
const legacyStatuses = new Map([['open', 'new'], ['pending', 'in_progress'], ['resolved', 'done']])
export function canonicalWorkStatus(status: string): string {return legacyStatuses.get(status) ?? status}
export function validDeadline(value?: number): value is number {return typeof value === 'number' && value > 0 && Number.isFinite(value) && Number.isFinite(new Date(value * 1000).getTime())}

// Buckets are exclusive: already elapsed times today belong to overdue, not
// today. Calendar construction keeps local-midnight and DST boundaries correct.
export function workDueBucket(item: TeamItem, now: Date): Exclude<DueFilter, 'all'> | 'completed' {
  if (canonicalWorkStatus(item.collab.status) === 'done') return 'completed'
  const due = item.collab.sla_due
  if (!validDeadline(due)) return 'none'
  if (due * 1000 < now.getTime()) return 'overdue'
  const tomorrow = new Date(now.getFullYear(), now.getMonth(), now.getDate() + 1)
  return due * 1000 < tomorrow.getTime() ? 'today' : 'upcoming'
}

export function workDueCounts(items: TeamItem[], now: Date): Record<DueFilter | 'completed', number> {
  const counts = {all: items.length, overdue: 0, today: 0, upcoming: 0, none: 0, completed: 0}
  for (const item of items) counts[workDueBucket(item, now)]++
  return counts
}

export function filterAndSortWork(items: TeamItem[], filter: DueFilter, order: WorkOrder, now: Date): TeamItem[] {
  return items.filter(item => filter === 'all' || workDueBucket(item, now) === filter).sort((left, right) => {
    if (order === 'deadline') {
      const leftDone = canonicalWorkStatus(left.collab.status) === 'done', rightDone = canonicalWorkStatus(right.collab.status) === 'done'
      if (leftDone !== rightDone) return leftDone ? 1 : -1
      if (!leftDone) {
        const leftDue = validDeadline(left.collab.sla_due) ? left.collab.sla_due : Infinity
        const rightDue = validDeadline(right.collab.sla_due) ? right.collab.sla_due : Infinity
        if (leftDue !== rightDue) return leftDue - rightDue
      }
    }
    return (right.collab.updated_at || 0) - (left.collab.updated_at || 0) || left.collab.message_id.localeCompare(right.collab.message_id)
  })
}
