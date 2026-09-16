import {afterEach, beforeEach, expect, it, vi} from 'vitest'
import {act, cleanup, render, screen, waitFor} from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import {QueryClient, QueryClientProvider} from '@tanstack/react-query'
import {MemoryRouter} from 'react-router-dom'
import {api} from '@/api/client'
import {DraftSourcePanel} from './DraftSourcePanel'

vi.mock('@/api/client', () => ({api: vi.fn()}))
const mockAPI = vi.mocked(api)
const source = {
  message: {id: 'source/one', account_id: 'other-owned-account', subject: '원본 계약 검토', from: {name: '김담당', email: 'sender@corp.local'}, to: [{email: 'me@corp.local'}], cc: [{email: 'review@corp.local'}], date: 1700000000},
  body: {text_body: '기한을 확인해 주세요. https://corp.local/review', html_sanitized: '<p>HTML 대신 텍스트로 확인</p><img src="https://tracker.invalid/pixel">', images_allowed: true},
  attachments: [{id: 'a1', name: 'contract.pdf', size: 300, scan_status: 'clean'}],
}
beforeEach(() => {mockAPI.mockReset()})
afterEach(() => cleanup())
function mount(props = {draftID: 'draft-one', kind: 'reply', messageID: 'source/one'}) {
  const cache = new QueryClient({defaultOptions: {queries: {retry: false}}})
  const content = (value: typeof props) => <QueryClientProvider client={cache}><MemoryRouter><DraftSourcePanel {...value}/></MemoryRouter></QueryClientProvider>
  const result = render(content(props))
  return {...result, change: (value: typeof props) => result.rerender(content(value))}
}

it.each([['reply', '답장 원문'], ['reply_all', '전체 답장 원문'], ['forward', '전달 원문']])('only reads %s source after expansion without changing read state or generating mail', async (kind, heading) => {
  mockAPI.mockResolvedValue(source)
  mount({draftID: 'draft-one', kind, messageID: 'source/one'})
  const user = userEvent.setup()
  expect(screen.getByRole('heading', {name: heading})).toBeInTheDocument()
  expect(screen.getByRole('button', {name: '원문 펼치기'})).toHaveAttribute('aria-expanded', 'false')
  expect(mockAPI).not.toHaveBeenCalled()
  await user.click(screen.getByRole('button', {name: '원문 펼치기'}))
  await screen.findByRole('heading', {name: '원본 계약 검토'})
  expect(mockAPI).toHaveBeenCalledExactlyOnceWith('/api/messages/source%2Fone', {signal: expect.any(AbortSignal)})
  expect(screen.getByText('김담당 <sender@corp.local>')).toBeInTheDocument()
  expect(screen.getByText('review@corp.local')).toBeInTheDocument()
  const original = screen.getByRole('link', {name: /원본 메일 새 탭에서 열기/})
  expect(original).toHaveAttribute('href', '/messages/source%2Fone')
  expect(original).toHaveAttribute('target', '_blank'); expect(original).toHaveAttribute('rel', 'noopener noreferrer')
  expect(screen.getByText(/원본 첨부 1개/)).toBeInTheDocument()
  expect(document.querySelector('img,iframe')).toBeNull()
  await user.click(screen.getByRole('button', {name: '원문 접기'}))
  expect(screen.queryByRole('heading', {name: '원본 계약 검토'})).not.toBeInTheDocument()
  expect(mockAPI).toHaveBeenCalledTimes(1)
})

it.each(['new', 'unknown', ''])('does not offer a source for unrelated draft kind %s', kind => {
  mount({draftID: 'draft-one', kind, messageID: 'source/one'})
  expect(screen.queryByRole('button')).toBeNull(); expect(mockAPI).not.toHaveBeenCalled()
})

it.each(['', ' ', '\n', 'x'.repeat(1025), undefined, {toString: 'PRIVATE_SOURCE'}])('never fetches malformed or missing references: %j', value => {
  mount({draftID: 'draft-one', kind: 'reply', messageID: value as string})
  expect(screen.getByRole('button', {name: '원문 펼치기'})).toBeDisabled()
  expect(screen.getByRole('alert')).toHaveTextContent('원본 메일 참조를 확인할 수 없습니다')
  expect(document.body).not.toHaveTextContent('PRIVATE_SOURCE'); expect(mockAPI).not.toHaveBeenCalled()
})

it('shows a safe local failure and permits retry without exposing provider error text', async () => {
  mockAPI.mockRejectedValueOnce(Object.assign(new Error('PRIVATE_SOURCE password=SECRET'), {status: 404})).mockResolvedValue(source)
  const user = userEvent.setup(); mount()
  await user.click(screen.getByRole('button', {name: '원문 펼치기'}))
  expect(await screen.findByRole('alert')).toHaveTextContent('초안은 계속 작성할 수 있습니다')
  expect(document.body).not.toHaveTextContent('PRIVATE_SOURCE'); expect(document.body).not.toHaveTextContent('SECRET')
  expect(mockAPI).toHaveBeenCalledTimes(1)
  await user.click(screen.getByRole('button', {name: '다시 시도'}))
  await screen.findByRole('heading', {name: '원본 계약 검토'})
  expect(mockAPI).toHaveBeenCalledTimes(2)
  expect(mockAPI.mock.calls.every(([path, options]) => path === '/api/messages/source%2Fone' && !options?.body && !options?.method)).toBe(true)
})

it.each([null, {message: null}, {...source, message: {...source.message, id: 'different-owner'}}, {...source, body: {text_body: {secret: 'PRIVATE_SOURCE'}}}])('refuses a malformed or mismatched source instead of displaying it: %j', payload => {
  mockAPI.mockResolvedValue(payload)
  mount()
  return userEvent.click(screen.getByRole('button', {name: '원문 펼치기'})).then(async () => {
    await screen.findByRole('alert')
    expect(screen.queryByRole('heading', {name: '원본 계약 검토'})).toBeNull()
    expect(document.body).not.toHaveTextContent('PRIVATE_SOURCE')
  })
})

it('isolates HTML-only original mail and ignores permission to load remote images', async () => {
  mockAPI.mockResolvedValue({...source, body: {...source.body, text_body: ''}})
  mount(); await userEvent.click(screen.getByRole('button', {name: '원문 펼치기'}))
  const frame = await screen.findByTitle('원본 메일 서식 미리보기')
  expect(frame).toHaveAttribute('sandbox', '')
  expect(frame).toHaveAttribute('referrerpolicy', 'no-referrer')
  expect(frame).not.toHaveAttribute('src')
  expect(frame.getAttribute('srcdoc')).toContain("img-src 'none'")
  expect(frame.getAttribute('srcdoc')).toContain("default-src 'none'")
  expect(mockAPI.mock.calls.some(([path]) => path.includes('external_images') || path.includes('/frame'))).toBe(false)
})

it('does not display a stale or unavailable body or invent an attachment download', async () => {
  mockAPI.mockResolvedValue({...source, body: {...source.body, unavailable: true, unavailable_reason: 'PRIVATE_SOURCE'}})
  mount(); await userEvent.click(screen.getByRole('button', {name: '원문 펼치기'}))
  expect(await screen.findByRole('status')).toHaveTextContent('원본 본문을 사용할 수 없습니다')
  expect(document.body).not.toHaveTextContent('PRIVATE_SOURCE')
  expect(screen.queryByText(/기한을 확인해 주세요/)).toBeNull()
  expect(document.querySelector('a[download],iframe,img')).toBeNull()
})

it('aborts an old source lookup and remounts collapsed when changing drafts and sources', async () => {
  let resolveOld!: (value: unknown) => void
  let signal!: AbortSignal
  mockAPI.mockImplementation((path, options) => {
    if (path.endsWith('source%2Fone')) {signal = options?.signal as AbortSignal; return new Promise(resolve => {resolveOld = resolve})}
    return Promise.resolve({...source, message: {...source.message, id: 'source-two', subject: '새 원문'}})
  })
  const panel = mount()
  await userEvent.click(screen.getByRole('button', {name: '원문 펼치기'}))
  await waitFor(() => expect(mockAPI).toHaveBeenCalledTimes(1))
  panel.change({draftID: 'draft-two', kind: 'forward', messageID: 'source-two'})
  expect(signal.aborted).toBe(true)
  expect(screen.getByRole('button', {name: '원문 펼치기'})).toHaveAttribute('aria-expanded', 'false')
  await act(async () => resolveOld(source))
  expect(screen.queryByRole('heading', {name: '원본 계약 검토'})).toBeNull()
  expect(mockAPI).toHaveBeenCalledTimes(1)
  await userEvent.click(screen.getByRole('button', {name: '원문 펼치기'}))
  await screen.findByRole('heading', {name: '새 원문'})
  expect(screen.queryByRole('heading', {name: '원본 계약 검토'})).toBeNull()
})
