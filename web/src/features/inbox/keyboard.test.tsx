import { afterEach, expect, it, vi } from 'vitest'
import { act, cleanup, render, screen } from '@testing-library/react'
import { MemoryRouter, useLocation } from 'react-router-dom'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { api } from '@/api/client'
import { dispatchMailCommand } from '@/lib/mail-commands'
import { InboxPage } from './InboxPage'

const scroll = vi.hoisted(() => vi.fn())
vi.mock('@/api/client', () => ({ api: vi.fn() }))
vi.mock('@/features/settings/preferences', () => ({ usePersonalPreferences: () => ({ value: (_: string, fallback: string) => fallback, locked: () => false }) }))
vi.mock('./SearchTools', () => ({ AdvancedSearch: () => null, BulkActions: () => null, keywordSearchParams: (params: URLSearchParams, cursor: string) => new URLSearchParams({ q: params.get('q') || '', account_id: params.get('account') || '', cursor }) }))
vi.mock('../messages/MessagePage', () => ({ MessagePane: ({ id }: { id: string }) => <div>상세 {id}</div> }))
vi.mock('react-virtuoso', async () => {
  const { forwardRef, useImperativeHandle } = await import('react')
  return { Virtuoso: forwardRef(({ data, itemContent }: { data: { id: string }[]; itemContent: (index: number, value: { id: string }) => React.ReactNode }, ref) => { useImperativeHandle(ref, () => ({ scrollIntoView: scroll })); return <div>{data.map((value, index) => <div key={value.id}>{itemContent(index, value)}</div>)}</div> }) }
})
function Location() { return <output aria-label="현재 경로">{useLocation().search}</output> }
afterEach(() => { cleanup(); vi.clearAllMocks() })
it('moves and scrolls only across loaded search results, preserving filters and clearing commands on exit', async () => {
  vi.mocked(api).mockResolvedValue({ messages: ['own1', 'own2'].map(id => ({ id, subject: id, account_id: 'own', from: { email: 'sender@corp.local' }, date: 1, created_at: 1, has_attachments: false })), next_cursor: 'unloaded-page' })
  const view = render(<QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}><MemoryRouter initialEntries={['/mail?q=contract&account=own']}><InboxPage/><Location/></MemoryRouter></QueryClientProvider>)
  await screen.findByText('own1')
  act(() => { expect(dispatchMailCommand('next')).toBe(true) }); await screen.findByText('상세 own1')
  expect(screen.getByLabelText('현재 경로')).toHaveTextContent('q=contract&account=own&message=own1')
  act(() => { expect(dispatchMailCommand('next')).toBe(true) }); await screen.findByText('상세 own2')
  expect(scroll).toHaveBeenLastCalledWith({ index: 1, behavior: 'auto' })
  act(() => { expect(dispatchMailCommand('next')).toBe(false) }); expect(api).toHaveBeenCalledTimes(1)
  act(() => { expect(dispatchMailCommand('previous')).toBe(true) }); await screen.findByText('상세 own1')
  view.unmount(); expect(dispatchMailCommand('next')).toBe(false); expect(dispatchMailCommand('previous')).toBe(false)
})
