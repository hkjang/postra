import { useSyncExternalStore } from 'react'

export type MailCommand = 'reply' | 'reply_all' | 'forward' | 'archive' | 'next' | 'previous' | 'close' | 'copy_context'
export const mailCommandLabels: Record<MailCommand, { label: string; key: string }> = {
  reply: { label: '현재 메일에 답장', key: 'R' },
  reply_all: { label: '현재 메일에 전체 답장', key: 'A' },
  forward: { label: '현재 메일 전달', key: 'F' },
  archive: { label: '현재 메일 보관', key: 'E' },
  next: { label: '불러온 다음 메일', key: 'J' },
  previous: { label: '불러온 이전 메일', key: 'K' },
  close: { label: '메일 상세 닫기', key: 'Esc' },
  copy_context: { label: '현재 메일 MCP Context 복사', key: '' },
}

// Only mounted, successfully authorized views register actions. The registry never
// stores message data, fetches other IDs, or grants permissions beyond those views.
const registrations = new Map<symbol, Partial<Record<MailCommand, () => void>>>()
const subscribers = new Set<() => void>()
let revision = 0
function changed() { revision++; subscribers.forEach(listener => listener()) }
export function registerMailCommands(handlers: Partial<Record<MailCommand, () => void>>) {
  const owner = Symbol('mail-view')
  registrations.set(owner, handlers); changed()
  return () => { registrations.delete(owner); changed() }
}
function handler(command: MailCommand) {
  for (const handlers of [...registrations.values()].reverse()) if (handlers[command]) return handlers[command]
}
export function dispatchMailCommand(command: MailCommand): boolean {
  const action = handler(command)
  if (!action) return false
  action(); return true
}
export function useAvailableMailCommands(): MailCommand[] {
  useSyncExternalStore(listener => { subscribers.add(listener); return () => { subscribers.delete(listener) } }, () => revision, () => 0)
  return (Object.keys(mailCommandLabels) as MailCommand[]).filter(command => !!handler(command))
}
