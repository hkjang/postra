export interface ActionCard {id: string; message_id: string; type: string; title: string; detail?: string; due?: string; assignee?: string; status: string; confidence?: number}
export type ActionGroup = 'today' | 'overdue' | 'upcoming' | 'waiting' | 'completed'
export const actionGroups: {id: ActionGroup; label: string}[] = [{id: 'today', label: '오늘'}, {id: 'overdue', label: '기한 초과'}, {id: 'upcoming', label: '예정'}, {id: 'waiting', label: '대기·기한 확인'}, {id: 'completed', label: '완료·제외'}]
export function actionGroup(card: ActionCard, now = new Date()): ActionGroup {
  if (['done', 'rejected'].includes(card.status)) return 'completed'
  let due: Date | undefined
  if (/^\d{4}-\d{2}-\d{2}$/.test(card.due || '')) {
    const [year, month, day] = card.due!.split('-').map(Number)
    const candidate = new Date(year, month - 1, day)
    if (candidate.getFullYear() === year && candidate.getMonth() === month - 1 && candidate.getDate() === day) due = candidate
  } else if (/^\d{4}-\d{2}-\d{2}T.+(?:Z|[+-]\d{2}:\d{2})$/i.test(card.due || '')) {
    const candidate = new Date(card.due!)
    if (Number.isFinite(candidate.getTime())) due = candidate
  }
  if (!due) return 'waiting'
  const start = new Date(now.getFullYear(), now.getMonth(), now.getDate())
  const end = new Date(now.getFullYear(), now.getMonth(), now.getDate() + 1)
  if (due < start) return 'overdue'
  return due < end ? 'today' : 'upcoming'
}
