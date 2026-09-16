import {act, cleanup, render, screen, waitFor} from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import {QueryClient, QueryClientProvider} from '@tanstack/react-query'
import {createMemoryRouter, Link, MemoryRouter, RouterProvider} from 'react-router-dom'
import {afterEach, beforeEach, expect, it, vi} from 'vitest'
import {api, APIError} from '@/api/client'
import {CreateActionForm} from './CreateActionForm'

vi.mock('@/api/client', async importOriginal => ({...await importOriginal<typeof import('@/api/client')>(), api: vi.fn()}))
vi.mock('sonner', () => ({toast: {success: vi.fn(), error: vi.fn()}}))
const first = {id: 'm1', account_id: 'a1', subject: '견적서 확인', from: {email: 'sender@corp.local'}, date: 1789462800}
const second = {...first, id: 'm2', subject: '일정 회신'}
const card = {id: 'act1', message_id: 'm1', type: 'todo', title: first.subject, status: 'pending'}
const writes = () => vi.mocked(api).mock.calls.filter(([, options]) => options?.method === 'POST')
beforeEach(() => {
  vi.clearAllMocks()
  vi.mocked(api).mockImplementation(async (path, options) => options?.method === 'POST' ? card : path === '/api/messages/m1?body=false' ? {message: first} : {messages: [first, second]})
})
afterEach(() => {cleanup(); vi.restoreAllMocks()})
function mount(initialID = '') {
  const close = vi.fn()
  const cache = new QueryClient({defaultOptions: {queries: {retry: false}, mutations: {retry: false}}})
  render(<QueryClientProvider client={cache}><MemoryRouter><CreateActionForm messageID={initialID} onClose={close}/></MemoryRouter></QueryClientProvider>)
  return {close, cache, person: userEvent.setup()}
}

it('selects owned mail without IDs, prefills its subject and only writes after explicit save', async () => {
  const {person, close} = mount()
  await person.click(await screen.findByRole('button', {name: '견적서 확인 · sender@corp.local 선택'}))
  expect(screen.getByLabelText('액션 제목')).toHaveValue(first.subject)
  expect(writes()).toHaveLength(0)
  await person.type(screen.getByLabelText('액션 담당자'), '홍담당')
  await person.click(screen.getByRole('button', {name: '액션 저장'}))
  await waitFor(() => expect(close).toHaveBeenCalledOnce())
  expect(writes()[0]).toEqual(['/api/action-cards', {method: 'POST', body: {message_id: 'm1', title: first.subject, detail: '', due: '', assignee: '홍담당', type: 'todo'}}])
})

it('preserves an edited title when selecting a different source, and never overwrites it during refetch', async () => {
  const {person, cache} = mount('m1')
  await waitFor(() => expect(screen.getByLabelText('액션 제목')).toHaveValue(first.subject))
  await person.clear(screen.getByLabelText('액션 제목'))
  await person.type(screen.getByLabelText('액션 제목'), '직접 작성한 후속 업무')
  await person.click(screen.getByRole('button', {name: '다른 메일 선택'}))
  await person.click(await screen.findByRole('button', {name: '일정 회신 · sender@corp.local 선택'}))
  act(() => cache.setQueryData(['action-source', 'm1'], {...first, subject: '늦은 새로고침'}))
  expect(screen.getByLabelText('액션 제목')).toHaveValue('직접 작성한 후속 업무')
  expect(screen.getByRole('link', {name: '원본 메일 보기 →'})).toHaveAttribute('href', '/mail?message=m2')
  expect(writes()).toHaveLength(0)
})

it('updates an untouched default title when switching messages', async () => {
  const {person} = mount('m1')
  await waitFor(() => expect(screen.getByLabelText('액션 제목')).toHaveValue(first.subject))
  await person.click(screen.getByRole('button', {name: '다른 메일 선택'}))
  await person.click(await screen.findByRole('button', {name: '일정 회신 · sender@corp.local 선택'}))
  expect(screen.getByLabelText('액션 제목')).toHaveValue(second.subject)
})

it('preserves text typed before the initial source request finishes', async () => {
  let resolve!: (value: unknown) => void
  vi.mocked(api).mockReturnValue(new Promise(done => {resolve = done}))
  const {person} = mount('m1')
  await person.type(screen.getByLabelText('액션 제목'), '먼저 쓴 제목')
  await act(async () => resolve({message: first}))
  expect(screen.getByLabelText('액션 제목')).toHaveValue('먼저 쓴 제목')
})

it('searches on Enter without submitting or mutating an action', async () => {
  const {person} = mount()
  await screen.findByRole('button', {name: '견적서 확인 · sender@corp.local 선택'})
  await person.type(screen.getByLabelText('원본 메일 찾기'), '견적 & 회신{Enter}')
  await waitFor(() => expect(vi.mocked(api).mock.calls.some(([path]) => path === '/api/messages?limit=10&q=%EA%B2%AC%EC%A0%81+%26+%ED%9A%8C%EC%8B%A0')).toBe(true))
  expect(writes()).toHaveLength(0)
})

it.each([new Error('원본 메일을 찾을 수 없습니다.'), {message: {...first, id: 'another-user-message'}}])('blocks saving when the source is inaccessible or mismatched', async outcome => {
  vi.mocked(api).mockImplementation(async path => {
    if (path === '/api/messages/m1?body=false') {if (outcome instanceof Error) throw outcome; return outcome}
    return {messages: [second]}
  })
  const {person} = mount('m1')
  await screen.findByRole('alert')
  expect(screen.getByRole('button', {name: '액션 저장'})).toBeDisabled()
  await person.click(screen.getByRole('button', {name: '다른 메일 찾기'}))
  await person.click(await screen.findByRole('button', {name: '일정 회신 · sender@corp.local 선택'}))
  expect(screen.getByLabelText('액션 제목')).toHaveValue(second.subject)
  expect(writes()).toHaveLength(0)
})

it('retains input after a failed save and blocks duplicate submission while pending', async () => {
  let reject!: (error: Error) => void
  vi.mocked(api).mockImplementation(async (path, options) => options?.method === 'POST' ? new Promise((_, fail) => {reject = fail}) : path === '/api/messages/m1?body=false' ? {message: first} : {messages: []})
  const {person, close} = mount('m1')
  await waitFor(() => expect(screen.getByLabelText('액션 제목')).toHaveValue(first.subject))
  await person.type(screen.getByLabelText('상세 내용'), '유지할 상세 내용')
  await person.dblClick(screen.getByRole('button', {name: '액션 저장'}))
  expect(writes()).toHaveLength(1)
  expect(screen.getByLabelText('액션 제목')).toBeDisabled()
  expect(screen.getByRole('button', {name: '닫기'})).toBeDisabled()
  await act(async () => reject(new APIError('입력을 확인해 주세요.', 400)))
  await screen.findByRole('alert')
  expect(screen.getByLabelText('상세 내용')).toHaveValue('유지할 상세 내용')
  expect(close).not.toHaveBeenCalled()
  expect(screen.getByRole('button', {name: '액션 저장'})).not.toBeDisabled()
})

it('does not claim success for a malformed creation response', async () => {
  vi.mocked(api).mockImplementation(async (_path, options) => options?.method === 'POST' ? null : {message: first})
  const {person, close} = mount('m1')
  await waitFor(() => expect(screen.getByLabelText('액션 제목')).toHaveValue(first.subject))
  await person.click(screen.getByRole('button', {name: '액션 저장'}))
  await screen.findByRole('alert')
  expect(close).not.toHaveBeenCalled()
  expect(screen.getByRole('button', {name: '액션 저장'})).toBeDisabled()
  expect(screen.getByText(/서버에 저장되었을 수 있어/)).toBeInTheDocument()
  await person.click(screen.getByRole('button', {name: '작성 닫고 액션 목록 확인'}))
  expect(close).toHaveBeenCalledWith(true)
})

it('confirms discard, protects logout and unlocks after a failed logout', async () => {
  const confirm = vi.spyOn(window, 'confirm').mockReturnValue(false)
  const {person, close} = mount('m1')
  await waitFor(() => expect(screen.getByLabelText('액션 제목')).toHaveValue(first.subject))
  await person.click(screen.getByRole('button', {name: '닫기'}))
  expect(close).not.toHaveBeenCalled()
  const canceled = new Event('postra:before-logout', {cancelable: true})
  act(() => window.dispatchEvent(canceled))
  expect(canceled.defaultPrevented).toBe(true)
  confirm.mockReturnValue(true)
  act(() => window.dispatchEvent(new Event('postra:before-logout', {cancelable: true})))
  expect(screen.getByLabelText('액션 제목')).toBeDisabled()
  act(() => window.dispatchEvent(new Event('postra:logout-failed')))
  expect(screen.getByLabelText('액션 제목')).not.toBeDisabled()
  expect(screen.getByLabelText('액션 제목')).toHaveValue(first.subject)
})

it('asks before leaving an unfinished action through the router', async () => {
  const cache = new QueryClient({defaultOptions: {queries: {retry: false}}})
  const router = createMemoryRouter([{path: '/', element: <><CreateActionForm messageID="m1" onClose={() => undefined}/><Link to="/elsewhere">다른 화면</Link></>}, {path: '/elsewhere', element: <h1>다른 화면 도착</h1>}])
  render(<QueryClientProvider client={cache}><RouterProvider router={router}/></QueryClientProvider>)
  await waitFor(() => expect(screen.getByLabelText('액션 제목')).toHaveValue(first.subject))
  await userEvent.click(screen.getByRole('link', {name: '다른 화면'}))
  await screen.findByRole('dialog', {name: '작성 중인 액션이 있습니다'})
  await userEvent.click(screen.getByRole('button', {name: '계속 작성'}))
  expect(screen.getByLabelText('액션 제목')).toHaveValue(first.subject)
  expect(screen.queryByRole('heading', {name: '다른 화면 도착'})).not.toBeInTheDocument()
})

it('recovers a source response received during a logout that subsequently fails', async () => {
  let resolve!: (value: unknown) => void
  vi.mocked(api).mockReturnValue(new Promise(done => {resolve = done}))
  mount('m1')
  act(() => window.dispatchEvent(new Event('postra:before-logout', {cancelable: true})))
  await act(async () => resolve({message: first}))
  expect(screen.getByRole('button', {name: '액션 저장'})).toBeDisabled()
  act(() => window.dispatchEvent(new Event('postra:logout-failed')))
  await waitFor(() => expect(screen.getByLabelText('액션 제목')).toHaveValue(first.subject))
  expect(screen.getByRole('button', {name: '액션 저장'})).not.toBeDisabled()
})
