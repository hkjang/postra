export type SnoozePreset = 'hour' | '1' | '3' | '7' | 'monday' | 'custom'
export const snoozeChoices: {value: SnoozePreset; label: string}[] = [
  {value: 'hour', label: '1시간 뒤'}, {value: '1', label: '내일 오전 9시'},
  {value: 'monday', label: '다음 월요일 오전 9시'}, {value: '3', label: '3일 후 오전 9시'},
  {value: '7', label: '일주일 후 오전 9시'}, {value: 'custom', label: '날짜·시각 직접 지정'},
]

// Calendar presets use the browser's local calendar rather than a fixed 24h
// offset. Strict component checks reject impossible dates and DST gaps.
export function snoozeTimestamp(preset: SnoozePreset, custom = '', now = new Date()): number {
  let due = new Date(now)
  if (preset === 'custom') {
    const fields = /^(\d{4})-(\d{2})-(\d{2})T(\d{2}):(\d{2})$/.exec(custom)
    if (!fields) throw new Error('다시 볼 날짜와 시각을 지정해 주세요.')
    const [year, month, day, hour, minute] = fields.slice(1).map(Number)
    due = new Date(0)
    due.setFullYear(year, month - 1, day)
    due.setHours(hour, minute, 0, 0)
    if (due.getFullYear() !== year || due.getMonth() !== month - 1 || due.getDate() !== day || due.getHours() !== hour || due.getMinutes() !== minute) {
      throw new Error('존재하지 않는 날짜·시각입니다. 현지 시간대를 확인해 주세요.')
    }
  } else if (preset === 'hour') {
    due = new Date(now.getTime() + 60 * 60 * 1000)
  } else if (preset === 'monday' || ['1', '3', '7'].includes(preset)) {
    const days = preset === 'monday' ? (8 - now.getDay()) % 7 || 7 : Number(preset)
    due.setDate(due.getDate() + days)
    due.setHours(9, 0, 0, 0)
  } else {
    throw new Error('다시 볼 시각을 선택해 주세요.')
  }
  if (!Number.isFinite(due.getTime()) || !Number.isFinite(now.getTime()) || due.getFullYear() > 9999 || due.getTime() <= now.getTime()) {
    throw new Error('현재보다 이후인 날짜와 시각을 지정해 주세요.')
  }
  return Math.floor(due.getTime() / 1000)
}

export function snoozeDateLabel(unix: number): string {
  return new Intl.DateTimeFormat('ko-KR', {dateStyle: 'medium', timeStyle: 'short'}).format(new Date(unix * 1000))
}
