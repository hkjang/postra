import {act, cleanup, render, screen, waitFor} from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import {QueryClient, QueryClientProvider} from '@tanstack/react-query'
import {MemoryRouter, useNavigate} from 'react-router-dom'
import {afterEach, beforeEach, describe, expect, it, vi} from 'vitest'
import {api} from '@/api/client'
import {ActionsPage} from './index'
import {actionGroup, type ActionCard} from './grouping'

vi.mock('@/api/client', async importOriginal => ({...await importOriginal<typeof import('@/api/client')>(), api: vi.fn()}))
vi.mock('sonner', () => ({toast: {success: vi.fn(), error: vi.fn()}}))
beforeEach(() => vi.clearAllMocks())
afterEach(() => {cleanup(); vi.restoreAllMocks(); vi.unstubAllGlobals()})
const card: ActionCard = {id: 'act1', message_id: 'm1', type: 'todo', title: '할 일', status: 'pending'}

describe('action due groups and explicit creation', () => {
  it.each([{}, null, {cards: {}}, {cards: [null]}, {cards: [{...card, title: {private: 'secret'}}]}, {cards: [{...card, due: []}]}])('shows a recoverable error instead of crashing or hiding malformed data: %j', async payload => {
    vi.mocked(api).mockResolvedValueOnce(payload).mockResolvedValue({cards: [card]})
    const cache = new QueryClient({defaultOptions: {queries: {retry: false}}})
    render(<QueryClientProvider client={cache}><MemoryRouter><ActionsPage/></MemoryRouter></QueryClientProvider>)
    expect(await screen.findByRole('alert')).toHaveTextContent('서버 응답 형식을 확인할 수 없습니다')
    expect(screen.queryByText('아직 등록된 액션이 없습니다')).not.toBeInTheDocument()
    expect(screen.queryByText('secret')).not.toBeInTheDocument()
    await userEvent.click(screen.getByRole('button', {name: '다시 시도'}))
    await screen.findByRole('heading', {name: '할 일'})
    expect(screen.getByRole('button', {name: '대기·기한 확인 (1)'})).toBeInTheDocument()
    expect(screen.queryByRole('alert')).not.toBeInTheDocument()
    expect(vi.mocked(api).mock.calls.every(([, options]) => !options?.body)).toBe(true)
  })

  it('keeps empty status filters and groups usable after existing actions', async () => {
    vi.mocked(api).mockImplementation(async path => path.endsWith('status=done') ? {cards: null} : {cards: [card]})
    const cache = new QueryClient({defaultOptions: {queries: {retry: false}}})
    render(<QueryClientProvider client={cache}><MemoryRouter><ActionsPage/></MemoryRouter></QueryClientProvider>)
    await screen.findByRole('heading', {name: '할 일'})
    await userEvent.selectOptions(screen.getByLabelText('승인·완료 상태'), 'done')
    await screen.findByText('조건에 맞는 액션이 없습니다')
    await userEvent.click(screen.getByRole('button', {name: '오늘 (0)'}))
    expect(screen.queryByRole('alert')).not.toBeInTheDocument()
    await userEvent.selectOptions(screen.getByLabelText('승인·완료 상태'), '')
    await userEvent.click(screen.getByRole('button', {name: '대기·기한 확인 (1)'}))
    await screen.findByRole('heading', {name: '할 일'})
  })

  it('renders the deployed empty-list payload without crashing the due counters', async () => {
    vi.mocked(api).mockResolvedValue({cards: null})
    const cache = new QueryClient({defaultOptions: {queries: {retry: false}}})
    render(<QueryClientProvider client={cache}><MemoryRouter><ActionsPage/></MemoryRouter></QueryClientProvider>)
    await screen.findByText('아직 등록된 액션이 없습니다')
    expect(screen.getByRole('button', {name: '오늘 (0)'})).toBeInTheDocument()
    expect(screen.getByRole('button', {name: '기한 초과 (0)'})).toBeInTheDocument()
    expect(screen.queryByRole('alert')).not.toBeInTheDocument()
  })

  it('uses actual calendar dates, keeps unknown AI dates waiting, and never treats export as completion', () => {
    const now = new Date(2026, 8, 15, 12)
    for (const [due, expected] of [['2026-09-15', 'today'], ['2026-09-14', 'overdue'], ['2026-09-16', 'upcoming'], ['', 'waiting'], ['다음 주 금요일', 'waiting'], ['2026-02-30', 'waiting']]) {
      expect(actionGroup({...card, due}, now)).toBe(expected)
    }
    expect(actionGroup({...card, status: 'done', due: '2026-09-14'}, now)).toBe('completed')
    expect(actionGroup({...card, status: 'exported', due: '2026-09-14'}, now)).toBe('overdue')
  })

  it('prefills the source mail title and saves a pending action only on explicit submit', async () => {
    vi.mocked(api).mockImplementation(async (path, options) => options?.method === 'POST' ? card : path === '/api/messages/m1?body=false' ? {message: {id: 'm1', account_id: 'a1', subject: '메일에서 가져온 제목', from: {email: 'sender@example.com'}}} : {cards: []})
    const cache = new QueryClient({defaultOptions: {queries: {retry: false}}})
    render(<QueryClientProvider client={cache}><MemoryRouter initialEntries={['/actions?message=m1&create=1']}><ActionsPage/></MemoryRouter></QueryClientProvider>)
    await screen.findByText('아직 등록된 액션이 없습니다')
    await waitFor(() => expect(screen.getByLabelText('액션 제목')).toHaveValue('메일에서 가져온 제목'))
    expect(screen.queryByLabelText('원본 메일 ID')).not.toBeInTheDocument()
    expect(vi.mocked(api).mock.calls.some(([, options]) => options?.method)).toBe(false)
    const person = userEvent.setup()
    await person.clear(screen.getByLabelText('액션 제목'))
    await person.type(screen.getByLabelText('액션 제목'), '예산 검토')
    await person.type(screen.getByLabelText('상세 내용'), '내부 확인 사항')
    await person.click(screen.getByRole('button', {name: '액션 저장'}))
    await waitFor(() => expect(api).toHaveBeenCalledWith('/api/action-cards', {method: 'POST', body: {message_id: 'm1', title: '예산 검토', detail: '내부 확인 사항', due: '', assignee: '', type: 'todo'}}))
    expect(vi.mocked(api).mock.calls.some(([path]) => path.includes('/analyze') || path.includes('/export'))).toBe(false)
  })

  it('searches title, detail and assignee locally with matching counts and a reset action', async () => {
    vi.mocked(api).mockResolvedValue({cards: [card, {...card, id: 'act2', title: '검토 회신', detail: '계약서 점검', assignee: '김담당'}]})
    const cache = new QueryClient({defaultOptions: {queries: {retry: false}}})
    render(<QueryClientProvider client={cache}><MemoryRouter><ActionsPage/></MemoryRouter></QueryClientProvider>)
    await screen.findByRole('heading', {name: '검토 회신'})
    const person = userEvent.setup()
    await person.type(screen.getByLabelText('액션 검색'), '김담당')
    expect(screen.queryByRole('heading', {name: '할 일'})).not.toBeInTheDocument()
    expect(screen.getByRole('button', {name: '대기·기한 확인 (1)'})).toBeInTheDocument()
    await person.clear(screen.getByLabelText('액션 검색'))
    await person.type(screen.getByLabelText('액션 검색'), '없는 업무')
    expect(screen.getByText('조건에 맞는 액션이 없습니다')).toBeInTheDocument()
    await person.click(screen.getByRole('button', {name: '모든 액션 보기'}))
    expect(screen.getByLabelText('액션 검색')).toHaveValue('')
    expect(screen.getByRole('heading', {name: '할 일'})).toBeInTheDocument()
    expect(screen.getByRole('button', {name: '대기·기한 확인 (2)'})).toBeInTheDocument()
    expect(api).toHaveBeenCalledTimes(1)
  })

  it.each(['__proto__', 'constructor', 'toString', 'unknown-value'])('uses safe labels for unknown action type and status %s', async value => {
    vi.mocked(api).mockResolvedValue({cards: [{...card, type: value, status: value}]})
    const cache = new QueryClient({defaultOptions: {queries: {retry: false}}})
    render(<QueryClientProvider client={cache}><MemoryRouter><ActionsPage/></MemoryRouter></QueryClientProvider>)
    await screen.findByRole('heading', {name: '할 일'})
    expect(screen.getByText('기타')).toBeInTheDocument()
    expect(screen.getByText('상태 확인 필요')).toBeInTheDocument()
    expect(screen.queryByRole('alert')).not.toBeInTheDocument()
    expect(screen.queryByRole('button', {name: '완료'})).not.toBeInTheDocument()
  })

  it.each(['export', 'status'])('serializes %s with other card writes even before a pending rerender', async first => {
    let resolve!: (value: unknown) => void
    const pending = new Promise<unknown>(done => {resolve = done})
    vi.mocked(api).mockImplementation(async (path, options) => options?.body ? pending : {cards: [{...card, status: 'approved'}]})
    vi.stubGlobal('URL', class extends URL {static createObjectURL() {return 'blob:action-test'}; static revokeObjectURL() {}})
    vi.spyOn(HTMLAnchorElement.prototype, 'click').mockImplementation(() => {})
    const cache = new QueryClient({defaultOptions: {queries: {retry: false}, mutations: {retry: false}}})
    render(<QueryClientProvider client={cache}><MemoryRouter><ActionsPage/></MemoryRouter></QueryClientProvider>)
    const exportButton = await screen.findByRole('button', {name: 'JSON 내보내기'})
    const doneButton = screen.getByRole('button', {name: '완료'})
    act(() => {
      ;(first === 'export' ? exportButton : doneButton).click()
      ;(first === 'export' ? doneButton : exportButton).click()
    })
    await waitFor(() => expect(api).toHaveBeenCalledWith(`/api/action-cards/act1/${first}`, {body: first === 'export' ? {target: 'calendar'} : {status: 'done'}}))
    expect(vi.mocked(api).mock.calls.filter(([, options]) => options?.body)).toHaveLength(1)
    await waitFor(() => expect(doneButton).toBeDisabled())
    expect(exportButton).toBeDisabled()
    await act(async () => {resolve({card: {...card, status: 'exported'}}); await pending})
    await waitFor(() => expect(doneButton).toBeEnabled())
    expect(exportButton).toBeEnabled()
    expect(vi.mocked(api).mock.calls.filter(([, options]) => options?.body)).toHaveLength(1)
  })

  it('releases the shared write lock after a failed request', async () => {
    vi.mocked(api).mockImplementation(async (_path, options) => {
      if (options?.body) throw new Error('요청 실패')
      return {cards: [{...card, status: 'approved'}]}
    })
    const cache = new QueryClient({defaultOptions: {queries: {retry: false}, mutations: {retry: false}}})
    render(<QueryClientProvider client={cache}><MemoryRouter><ActionsPage/></MemoryRouter></QueryClientProvider>)
    const exportButton = await screen.findByRole('button', {name: 'JSON 내보내기'})
    await userEvent.click(exportButton)
    await waitFor(() => expect(screen.getByRole('button', {name: '완료'})).toBeEnabled())
    await userEvent.click(screen.getByRole('button', {name: '완료'}))
    await waitFor(() => expect(vi.mocked(api).mock.calls.filter(([, options]) => options?.body)).toHaveLength(2))
  })

  it('opens a same-mounted URL intent without resetting ongoing edits or reopening after close', async () => {
    const message = {id: 'm1', account_id: 'a1', subject: '원본 메일', from: {email: 'sender@example.com'}}
    vi.mocked(api).mockImplementation(async path => path === '/api/messages/m1?body=false' ? {message} : {cards: []})
    vi.spyOn(window, 'confirm').mockReturnValue(true)
    function NavigateIntent() {
      const navigate = useNavigate()
      return <><button onClick={() => navigate('/actions?message=m1&create=1')}>작성 URL 열기</button><button onClick={() => navigate('/actions?message=m1&create=1&view=recent')}>작성 URL 갱신</button><ActionsPage/></>
    }
    const cache = new QueryClient({defaultOptions: {queries: {retry: false}}})
    render(<QueryClientProvider client={cache}><MemoryRouter initialEntries={['/actions']}><NavigateIntent/></MemoryRouter></QueryClientProvider>)
    await screen.findByText('아직 등록된 액션이 없습니다')
    expect(screen.queryByLabelText('액션 제목')).not.toBeInTheDocument()
    const person = userEvent.setup()
    await person.click(screen.getByRole('button', {name: '작성 URL 열기'}))
    await waitFor(() => expect(screen.getByLabelText('액션 제목')).toHaveValue('원본 메일'))
    await person.type(screen.getByLabelText('상세 내용'), '작성 중인 내용')
    await person.click(screen.getByRole('button', {name: '작성 URL 갱신'}))
    expect(screen.getByLabelText('상세 내용')).toHaveValue('작성 중인 내용')
    await person.click(screen.getByRole('button', {name: '닫기'}))
    expect(screen.queryByLabelText('액션 제목')).not.toBeInTheDocument()
    await person.type(screen.getByLabelText('액션 검색'), '필터 변경')
    expect(screen.queryByLabelText('액션 제목')).not.toBeInTheDocument()
    await person.click(screen.getByRole('button', {name: '작성 URL 열기'}))
    expect(await screen.findByLabelText('액션 제목')).toBeInTheDocument()
    expect(screen.getByLabelText('상세 내용')).toHaveValue('')
  })
})
