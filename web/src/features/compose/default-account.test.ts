import { expect, it } from 'vitest'
import { defaultComposeAccount } from './default-account'

const accounts = [{ id: 'first', email: 'first@corp.local', name: 'first', status: 'active' }, { id: 'preferred', email: 'preferred@corp.local', name: 'preferred', status: 'active' }, { id: 'disabled', email: 'old@corp.local', name: 'old', status: 'disabled' }]
it('selects only owned active accounts, respecting explicit then personal choices', () => {
  expect(defaultComposeAccount(accounts, 'preferred')).toBe('preferred')
  expect(defaultComposeAccount(accounts, 'preferred', 'first')).toBe('first')
  expect(defaultComposeAccount(accounts, 'foreign')).toBe('first')
  expect(defaultComposeAccount(accounts, 'disabled')).toBe('first')
  expect(defaultComposeAccount(accounts, 'preferred', 'foreign')).toBe('preferred')
  expect(defaultComposeAccount([accounts[2]], 'disabled')).toBe('')
})
