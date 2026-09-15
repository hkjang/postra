import {clsx, type ClassValue} from 'clsx'
import {twMerge} from 'tailwind-merge'

export function cn(...values: ClassValue[]) { return twMerge(clsx(values)) }
export function formatDate(value?: number) {
  if (!value) return '—'
  const language = document.documentElement.lang === 'en' ? 'en-US' : 'ko-KR'
  const mode = document.documentElement.dataset.dateFormat
  const date = new Date(value * 1000)
  if (mode === 'iso') return date.toISOString().replace('T', ' ').slice(0, 19) + ' UTC'
  if (mode === 'relative') {
    const seconds = Math.round((date.getTime() - Date.now()) / 1000)
    const relative = new Intl.RelativeTimeFormat(language, {numeric: 'auto'})
    if (Math.abs(seconds) < 60) return relative.format(seconds, 'second')
    if (Math.abs(seconds) < 3600) return relative.format(Math.round(seconds / 60), 'minute')
    if (Math.abs(seconds) < 86400) return relative.format(Math.round(seconds / 3600), 'hour')
    if (Math.abs(seconds) < 604800) return relative.format(Math.round(seconds / 86400), 'day')
  }
  return new Intl.DateTimeFormat(language, {month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit', year: date.getFullYear() === new Date().getFullYear() ? undefined : 'numeric'}).format(date)
}
