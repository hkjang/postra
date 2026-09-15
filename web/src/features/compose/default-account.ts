import type { Account } from '../messages/types'

export function defaultComposeAccount(accounts: Account[], preferred: string, explicit = ''): string {
  const active = accounts.filter(account => account.status === 'active')
  return active.find(account => account.id === explicit)?.id || active.find(account => account.id === preferred)?.id || active[0]?.id || ''
}
