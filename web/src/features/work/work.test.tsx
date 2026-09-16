import {cleanup, fireEvent, render, screen, waitFor, within} from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import {QueryClient, QueryClientProvider} from '@tanstack/react-query'
import {MemoryRouter} from 'react-router-dom'
import {afterEach, beforeEach, expect, it, vi} from 'vitest'
import {api} from '@/api/client'
import {SessionContext} from '@/app/session'
import {TeamPage, canonicalWorkStatus} from './index'
import type {TeamItem} from './deadlines'

vi.mock('@/api/client', () => ({api: vi.fn()}))
vi.mock('sonner', () => ({toast: {success: vi.fn(), error: vi.fn()}}))
beforeEach(() => {vi.clearAllMocks(); vi.useFakeTimers({toFake: ['Date']}); vi.setSystemTime(new Date(2026, 8, 16, 12, 30))})
afterEach(() => {cleanup(); vi.useRealTimers()})

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

const item = (id: string, due?: number, status = 'new', updated = 1, assignee = 'hong'): TeamItem => ({collab: {message_id: id, sla_due: due, status, updated_at: updated, assignee}, message: {id, account_id: 'a1', subject: id, from: {email: 'sender@corp.local'}, date: 1, created_at: 1, has_attachments: false}})
function mountItems(items: TeamItem[]) {
  vi.mocked(api).mockImplementation(async (path, options) => {
    if (path.endsWith('/collab')) {const id = decodeURIComponent(path.split('/')[3]); return {collab: items.find(value => value.collab.message_id === id)!.collab, notes: []}}
    if (options?.method === 'POST') return {}
    const params = new URL(path, 'https://postra.test').searchParams
    return {items: items.filter(value => (!params.get('status') || canonicalWorkStatus(value.collab.status) === params.get('status')) && (!params.get('assignee') || value.collab.assignee === params.get('assignee')))}
  })
  const cache = new QueryClient({defaultOptions: {queries: {retry: false}}})
  render(<QueryClientProvider client={cache}><SessionContext.Provider value={{user_id: 'u1', login_id: 'hong', display_name: '홍', role: 'user', auth_method: 'local'}}><MemoryRouter><TeamPage/></MemoryRouter></SessionContext.Provider></QueryClientProvider>)
}

it('shows scoped counts and filters the actual board without additional deadline API requests', async () => {
  const now = Date.now() / 1000, tomorrow = new Date(2026, 8, 17).getTime() / 1000
  mountItems([item('지연 업무', now - 1), item('오늘 업무', now), item('예정 업무', tomorrow), item('날짜 없는 업무'), item('완료 업무', now - 100, 'done'), item('이전 완료 업무', now - 100, 'resolved')])
  const user = userEvent.setup()
  await screen.findByText('표시 6 / 불러온 6건 · 완료 2건')
  const filters = within(screen.getByRole('group', {name: '업무 기한 필터'}))
  expect(filters.getByRole('button', {name: /^전체\s*6$/})).toHaveAttribute('aria-pressed', 'true')
  expect(screen.getByText(/최근 변경된 최대 200건만/)).toBeInTheDocument()
  for (const [name, title] of [['지연', '지연 업무'], ['오늘', '오늘 업무'], ['예정', '예정 업무'], ['기한 없음', '날짜 없는 업무']]) {
    await user.click(filters.getByRole('button', {name: new RegExp(`^${name}\\s*1$`)}))
    expect(screen.getByRole('heading', {name: title})).toBeInTheDocument()
    expect(within(screen.getByRole('region', {name: '업무 상태 보드'})).getAllByRole('button')).toHaveLength(1)
    expect(within(screen.getByRole('region', {name: '업무 상태 보드'})).getAllByRole('heading', {level: 2})).toHaveLength(1)
    expect(screen.queryByRole('region', {name: '완료'})).not.toBeInTheDocument()
    expect(screen.queryByRole('heading', {name: '완료 업무'})).not.toBeInTheDocument()
    expect(screen.getByRole('status')).toHaveTextContent('표시 1 / 불러온 6건')
  }
  expect(vi.mocked(api).mock.calls).toHaveLength(1)
  const all = filters.getByRole('button', {name: /^전체\s*6$/})
  all.focus(); await user.keyboard(' ')
  expect(screen.getByRole('heading', {name: '완료 업무'})).toBeInTheDocument()
  expect(within(screen.getByRole('region', {name: '업무 상태 보드'})).getAllByRole('heading', {level: 2})).toHaveLength(5)
  expect(screen.getByRole('status')).toHaveTextContent('표시 6 / 불러온 6건')
})

it('sorts each active column by deadline while retaining recent-first completed order', async () => {
  const now = Date.now() / 1000
  mountItems([item('날짜 없음', undefined, 'new', 10), item('늦은 기한', now + 100, 'new', 9), item('빠른 기한', now - 100, 'new', 8), item('오래된 완료', now - 500, 'done', 1), item('최근 완료', now - 100, 'resolved', 2)])
  const user = userEvent.setup(); await screen.findByText('표시 5 / 불러온 5건 · 완료 2건')
  await user.selectOptions(screen.getByRole('combobox', {name: '업무 정렬'}), 'deadline')
  const titles = (region: string) => within(screen.getByRole('region', {name: region})).getAllByRole('heading', {level: 3}).map(node => node.textContent)
  expect(titles('새로 접수')).toEqual(['빠른 기한', '늦은 기한', '날짜 없음'])
  expect(titles('완료')).toEqual(['최근 완료', '오래된 완료'])
  expect(within(screen.getByRole('region', {name: '완료'})).queryByText(/기한 지남/)).not.toBeInTheDocument()
})

it('provides a clear empty-filter result and resets assignee, status, deadline and sorting together', async () => {
  mountItems([item('완료만 있는 업무', 1, 'done')])
  const user = userEvent.setup(); await screen.findByText('표시 1 / 불러온 1건 · 완료 1건')
  await user.click(screen.getByRole('button', {name: '내 담당'}))
  await user.selectOptions(screen.getByRole('combobox', {name: '상태 필터'}), 'done')
  await waitFor(() => expect(within(screen.getByRole('region', {name: '업무 상태 보드'})).getAllByRole('heading', {level: 2})).toHaveLength(1))
  await user.selectOptions(screen.getByRole('combobox', {name: '업무 정렬'}), 'deadline')
  await user.click(await screen.findByRole('button', {name: /^지연\s*0$/}))
  expect(await screen.findByText('조건에 맞는 업무가 없습니다')).toBeInTheDocument()
  expect(screen.getByRole('status')).toHaveTextContent('표시 0 / 불러온 1건')
  await user.click(screen.getByRole('button', {name: '전체 업무 보기'}))
  await screen.findByRole('heading', {name: '완료만 있는 업무'})
  expect(screen.getByLabelText('담당자 필터')).toHaveValue('')
  expect(screen.getByLabelText('상태 필터')).toHaveValue('')
  expect(screen.getByLabelText('업무 정렬')).toHaveValue('recent')
  expect(screen.getByRole('button', {name: /^전체\s*1$/})).toHaveAttribute('aria-pressed', 'true')
  expect(within(screen.getByRole('region', {name: '업무 상태 보드'})).getAllByRole('heading', {level: 2})).toHaveLength(5)
})

it('encodes source IDs and supports keyboard selection without stealing card arrow keys', async () => {
  const id = '메일/원본?x=# a'
  mountItems([item(id)])
  const user = userEvent.setup(); const board = await screen.findByRole('region', {name: '업무 상태 보드'})
  const scrollBy = vi.fn(); Object.defineProperty(board, 'scrollBy', {value: scrollBy})
  board.focus(); await user.keyboard('{ArrowRight}')
  expect(scrollBy).toHaveBeenCalledWith({left: 260, behavior: 'auto'})
  const card = within(board).getByRole('button')
  card.focus(); await user.keyboard('{ArrowLeft}')
  expect(scrollBy).toHaveBeenCalledTimes(1)
  await user.keyboard('{Enter}')
  expect(await screen.findByRole('complementary', {name: '업무 처리 관리'})).toBeInTheDocument()
  expect(card).toHaveAttribute('aria-pressed', 'true')
  expect(api).toHaveBeenCalledWith(`/api/messages/${encodeURIComponent(id)}/collab`, expect.objectContaining({signal: expect.any(AbortSignal)}))
  expect(screen.getByRole('link', {name: '원본 메일 열기 →'})).toHaveAttribute('href', `/mail?message=${encodeURIComponent(id)}`)
  await user.selectOptions(screen.getByRole('combobox', {name: '처리 상태'}), 'waiting')
  await waitFor(() => expect(api).toHaveBeenCalledWith(`/api/messages/${encodeURIComponent(id)}/work-status`, {method: 'POST', body: {status: 'waiting'}}))
})

it('refreshes local deadline groups on window focus without changing server filters', async () => {
  const due = Date.now() / 1000 + 30
  mountItems([item('곧 마감', due)])
  await screen.findByRole('button', {name: /^오늘\s*1$/})
  vi.setSystemTime(new Date((due + 1) * 1000))
  fireEvent(window, new Event('focus'))
  expect(screen.getByRole('button', {name: /^지연\s*1$/})).toBeInTheDocument()
  expect(screen.getByRole('button', {name: /^오늘\s*0$/})).toBeInTheDocument()
})
