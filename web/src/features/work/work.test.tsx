import {cleanup, render, screen, waitFor, within} from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import {QueryClient, QueryClientProvider} from '@tanstack/react-query'
import {MemoryRouter} from 'react-router-dom'
import {afterEach, beforeEach, expect, it, vi} from 'vitest'
import {api} from '@/api/client'
import {TeamPage, canonicalWorkStatus} from './index'

vi.mock('@/api/client', () => ({api: vi.fn()}))
vi.mock('sonner', () => ({toast: {success: vi.fn(), error: vi.fn()}}))
beforeEach(() => vi.clearAllMocks())
afterEach(() => cleanup())

it('maps legacy states and explicitly persists a five-state transition without mutating on render', async () => {
  expect(canonicalWorkStatus('open')).toBe('new')
  expect(canonicalWorkStatus('pending')).toBe('in_progress')
  expect(canonicalWorkStatus('resolved')).toBe('done')
  let status = 'pending'
  const collab = () => ({message_id: 'm1', status, assignee: 'hong', updated_at: 1})
  vi.mocked(api).mockImplementation(async (path, options) => {
    if (path.endsWith('/work-status')) {status = (options?.body as {status: string}).status; return collab()}
    if (path.endsWith('/collab')) return {collab: collab(), notes: []}
    return {items: [{collab: collab(), message: {id: 'm1', subject: '현재 업무', from: {email: 'a@corp.local'}}}]}
  })
  const cache = new QueryClient({defaultOptions: {queries: {retry: false}}})
  render(<QueryClientProvider client={cache}><MemoryRouter initialEntries={['/team?message=m1']}><TeamPage/></MemoryRouter></QueryClientProvider>)
  await screen.findByRole('combobox', {name: '처리 상태'})
  expect(screen.getByRole('combobox', {name: '처리 상태'})).toHaveValue('in_progress')
  expect(within(screen.getByRole('region', {name: '진행 중'})).getByText('현재 업무')).toBeInTheDocument()
  expect(vi.mocked(api).mock.calls.some(([, options]) => options?.method)).toBe(false)
  await userEvent.setup().selectOptions(screen.getByRole('combobox', {name: '처리 상태'}), 'waiting')
  await waitFor(() => expect(api).toHaveBeenCalledWith('/api/messages/m1/work-status', {method: 'POST', body: {status: 'waiting'}}))
  expect(screen.getByRole('link', {name: '이 메일에서 액션 만들기 →'})).toHaveAttribute('href', '/actions?message=m1&create=1')
})
