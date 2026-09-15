import {cleanup, render, screen, waitFor} from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import {QueryClient, QueryClientProvider} from '@tanstack/react-query'
import {MemoryRouter} from 'react-router-dom'
import {afterEach, beforeEach, describe, expect, it, vi} from 'vitest'
import {api} from '@/api/client'
import {ActionsPage} from './index'
import {actionGroup, type ActionCard} from './grouping'

vi.mock('@/api/client', () => ({api: vi.fn()}))
vi.mock('sonner', () => ({toast: {success: vi.fn(), error: vi.fn()}}))
beforeEach(() => vi.clearAllMocks())
afterEach(() => cleanup())
const card: ActionCard = {id: 'act1', message_id: 'm1', type: 'todo', title: '할 일', status: 'pending'}

describe('action due groups and explicit creation', () => {
  it('uses actual calendar dates, keeps unknown AI dates waiting, and never treats export as completion', () => {
    const now = new Date(2026, 8, 15, 12)
    for (const [due, expected] of [['2026-09-15', 'today'], ['2026-09-14', 'overdue'], ['2026-09-16', 'upcoming'], ['', 'waiting'], ['다음 주 금요일', 'waiting'], ['2026-02-30', 'waiting']]) {
      expect(actionGroup({...card, due}, now)).toBe(expected)
    }
    expect(actionGroup({...card, status: 'done', due: '2026-09-14'}, now)).toBe('completed')
    expect(actionGroup({...card, status: 'exported', due: '2026-09-14'}, now)).toBe('overdue')
  })

  it('prefills only the source mail and saves a pending action only on explicit submit', async () => {
    vi.mocked(api).mockImplementation(async (_path, options) => options?.method === 'POST' ? card : {cards: []})
    const cache = new QueryClient({defaultOptions: {queries: {retry: false}}})
    render(<QueryClientProvider client={cache}><MemoryRouter initialEntries={['/actions?message=m1&create=1']}><ActionsPage/></MemoryRouter></QueryClientProvider>)
    await screen.findByText('아직 등록된 액션이 없습니다')
    expect(screen.getByLabelText('원본 메일 ID')).toHaveValue('m1')
    expect(vi.mocked(api).mock.calls.some(([, options]) => options?.method)).toBe(false)
    const person = userEvent.setup()
    await person.type(screen.getByLabelText('액션 제목'), '예산 검토')
    await person.type(screen.getByLabelText('상세 내용'), '내부 확인 사항')
    await person.click(screen.getByRole('button', {name: '액션 저장'}))
    await waitFor(() => expect(api).toHaveBeenCalledWith('/api/action-cards', {method: 'POST', body: {message_id: 'm1', title: '예산 검토', detail: '내부 확인 사항', due: '', assignee: '', type: 'todo'}}))
    expect(vi.mocked(api).mock.calls.some(([path]) => path.includes('/analyze') || path.includes('/export'))).toBe(false)
  })
})
