import {afterEach, beforeEach, expect, it, vi} from 'vitest'
import {act, cleanup, fireEvent, render, screen, waitFor} from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import {QueryClient, QueryClientProvider} from '@tanstack/react-query'
import {createMemoryRouter, RouterProvider} from 'react-router-dom'
import {api} from '@/api/client'
import {ComposePage} from './ComposePage'

vi.mock('@/api/client', () => ({api: vi.fn()}))
vi.mock('./RichEditor', () => ({RichEditor: ({value, onChange}: {value: string; onChange: (html: string, text: string) => void}) => <textarea aria-label="메일 본문 서식 편집기" value={value} onChange={event => onChange(event.target.value, '내 답장')}/>}))
const initial = {draft: {id: 'd1', account_id: 'own', status: 'open', kind: 'reply', reply_to_message_id: 'source1', current_version: 1, updated_at: 1}, version: {version: 1, subject: '답장 제목', to: [{email: 'sender@corp.local'}], body_text: '내 답장', body_html: '<p>내 답장</p>', author: 'user'}}
const source = {message: {id: 'source1', account_id: 'other-owned-account', subject: '원본 제목', from: {email: 'sender@corp.local'}, date: 1}, body: {text_body: '원본 참고 내용'}}
let sourceFailed = false
beforeEach(() => {
  sourceFailed = false; vi.mocked(api).mockReset()
  vi.mocked(api).mockImplementation(async path => {
    if (path === '/api/accounts') return [{id: 'own', email: 'me@corp.local', status: 'active'}]
    if (path === '/api/signatures' || path === '/api/mail/templates') return []
    if (path.endsWith('/preferences')) return {fields: []}
    if (path === '/api/drafts/d1') return initial
    if (path === '/api/drafts/d2') return {...initial, draft: {...initial.draft, id: 'd2', kind: 'forward', reply_to_message_id: 'source2'}}
    if (path === '/api/messages/source1') {if (sourceFailed) throw Object.assign(new Error('PRIVATE_SOURCE'), {status: 403}); return source}
    if (path === '/api/messages/source2') return {...source, message: {...source.message, id: 'source2', subject: '새 원본 제목'}}
    throw new Error('Unexpected request')
  })
})
afterEach(() => cleanup())
function mount() {
  const cache = new QueryClient({defaultOptions: {queries: {retry: false}, mutations: {retry: false}}})
  const router = createMemoryRouter([{path: '/drafts/:id', element: <ComposePage/>}], {initialEntries: ['/drafts/d1']})
  render(<QueryClientProvider client={cache}><RouterProvider router={router}/></QueryClientProvider>)
  return router
}

it.each([false, true])('preserves unsaved editor content while reading the source (source failure=%s)', async failed => {
  sourceFailed = failed; mount()
  await screen.findByDisplayValue('답장 제목')
  expect(vi.mocked(api).mock.calls.some(([path]) => path.startsWith('/api/messages/'))).toBe(false)
  fireEvent.change(screen.getByLabelText('메일 제목'), {target: {value: '저장 전 내 제목'}})
  fireEvent.change(screen.getByLabelText('메일 본문 서식 편집기'), {target: {value: '<p>저장 전 내 본문</p>'}})
  await userEvent.click(screen.getByRole('button', {name: '원문 펼치기'}))
  if (failed) expect(await screen.findByRole('alert')).toHaveTextContent('초안은 계속 작성할 수 있습니다')
  else await screen.findByRole('heading', {name: '원본 제목'})
  expect(screen.getByLabelText('메일 제목')).toHaveValue('저장 전 내 제목')
  expect(screen.getByLabelText('메일 본문 서식 편집기')).toHaveValue('<p>저장 전 내 본문</p>')
  expect(screen.getByText('저장하지 않은 변경')).toBeInTheDocument()
  expect(screen.getByRole('button', {name: '초안 저장'})).toBeEnabled()
  expect(document.body).not.toHaveTextContent('PRIVATE_SOURCE')
  expect(vi.mocked(api).mock.calls.every(([, options]) => !options?.body && !options?.method)).toBe(true)
})

it('hides the previous original when navigating to another draft and waits for explicit expansion', async () => {
  const router = mount()
  await screen.findByDisplayValue('답장 제목')
  await userEvent.click(screen.getByRole('button', {name: '원문 펼치기'}))
  await screen.findByRole('heading', {name: '원본 제목'})
  await act(async () => router.navigate('/drafts/d2'))
  await screen.findByRole('heading', {name: '전달 원문'})
  expect(screen.queryByRole('heading', {name: '원본 제목'})).toBeNull()
  expect(screen.getByRole('button', {name: '원문 펼치기'})).toHaveAttribute('aria-expanded', 'false')
  expect(vi.mocked(api).mock.calls.some(([path]) => path === '/api/messages/source2')).toBe(false)
  await userEvent.click(screen.getByRole('button', {name: '원문 펼치기'}))
  await waitFor(() => expect(screen.getByRole('heading', {name: '새 원본 제목'})).toBeInTheDocument())
})
