import {clsx, type ClassValue} from 'clsx'
import {twMerge} from 'tailwind-merge'

export function cn(...values: ClassValue[]) { return twMerge(clsx(values)) }
export function formatDate(value?: number) {
  if (!value) return '—'
  return new Intl.DateTimeFormat('ko-KR', {month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit'}).format(new Date(value * 1000))
}
