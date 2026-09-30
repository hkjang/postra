import {afterEach, expect, it, vi} from 'vitest'
import {cleanup, render, screen} from '@testing-library/react'
import {QueryClient, QueryClientProvider} from '@tanstack/react-query'
import {MemoryRouter, Route, Routes} from 'react-router-dom'
import {api} from '@/api/client'
import {navSelected} from '@/components/layout/Workspace'
import {InboxPage, mailCounterpart} from './InboxPage'
import {keywordSearchParams} from './SearchTools'
import {isSentView} from './views'
import type {Message} from '../messages/types'

vi.mock('@/api/client', () => ({api: vi.fn()}))
vi.mock('../messages/MessagePage', () => ({MessagePane: () => null}))
// jsdom has no layout, so the virtual list would render no rows.
vi.mock('react-virtuoso', async () => {
  const {forwardRef} = await import('react')
  return {Virtuoso: forwardRef(({data, itemContent}: {data: {id: string}[]; itemContent: (index: number, value: {id: string}) => React.ReactNode}, _ref) => <div>{data.map((value, index) => <div key={value.id}>{itemContent(index, value)}</div>)}</div>)}
})
vi.mock('@/features/settings/usePreferenceBridge', () => ({usePreferenceBridge: () => ({data: {}, value: (_: string, fallback: string) => fallback, locked: () => false, update: vi.fn()})}))
afterEach(() => {cleanup(); vi.restoreAllMocks()})

const sent: Message = {id: 'msg_s', account_id: 'acc', subject: '견적 회신', from: {email: 'me@corp.local'}, to: [{email: 'bob@partner.test', name: 'Bob'}, {email: 'amy@partner.test'}], cc: [{email: 'lee@corp.local'}], date: 1780000000, created_at: 1780000000, has_attachments: false, is_read: true, mailbox: 'sent'}

it('asks the API for sent mail, and for what awaits a reply', () => {
  expect(Object.fromEntries(keywordSearchParams(new URLSearchParams('folder=sent'), ''))).toMatchObject({folder: 'sent'})
  expect(keywordSearchParams(new URLSearchParams('folder=sent'), '').has('awaiting_reply')).toBe(false)
  expect(Object.fromEntries(keywordSearchParams(new URLSearchParams('folder=awaiting&to=bob'), 'c1'))).toMatchObject({folder: 'sent', awaiting_reply: 'true', to: 'bob', cursor: 'c1'})
  // Received-mail views are unchanged and never ask for sent mail.
  for (const folder of ['inbox', 'important', 'unread', 'today', 'bogus']) {
    expect(keywordSearchParams(new URLSearchParams('folder=' + folder), '').get('folder')).not.toBe('sent')
  }
  expect(isSentView('awaiting') && isSentView('sent') && !isSentView('inbox')).toBe(true)
})

it('selects the sent item on both sent views and the inbox item on neither', () => {
  for (const search of ['?folder=sent', '?folder=awaiting']) {
    expect(navSelected('/mail?folder=sent', '/mail', search)).toBe(true)
    expect(navSelected('/mail', '/mail', search)).toBe(false)
  }
  expect(navSelected('/mail', '/mail', '')).toBe(true)
  expect(navSelected('/mail', '/mail', '?folder=archive')).toBe(true)
  expect(navSelected('/mail?folder=important', '/mail', '?folder=important')).toBe(true)
  expect(navSelected('/mail', '/mail', '?folder=important')).toBe(false)
  expect(navSelected('/sent', '/sent', '')).toBe(true)
  expect(navSelected('/mail?folder=sent', '/sent', '')).toBe(false)
})

it('names the recipients of sent mail and the sender of received mail', () => {
  expect(mailCounterpart(sent)).toBe('받는 사람: Bob, amy@partner.test 외 1명')
  expect(mailCounterpart({...sent, mailbox: 'inbox', from: {name: 'Kim', email: 'kim@x'}})).toBe('Kim')
  expect(mailCounterpart({...sent, to: [], cc: []})).toBe('받는 사람 없음')
})

it('shows the sent view with its own tabs, recipients and keyword search only', async () => {
  vi.mocked(api).mockResolvedValue({messages: [sent]})
  render(<QueryClientProvider client={new QueryClient({defaultOptions: {queries: {retry: false}}})}><MemoryRouter initialEntries={['/mail?folder=awaiting&mode=hybrid']}><Routes><Route path="/mail" element={<InboxPage/>}/></Routes></MemoryRouter></QueryClientProvider>)
  expect(await screen.findByText('받는 사람: Bob, amy@partner.test 외 1명')).toBeInTheDocument()
  expect(screen.getByRole('heading', {name: '답장 대기'})).toBeInTheDocument()
  expect(screen.getByRole('button', {name: '답장 대기', pressed: true})).toBeInTheDocument()
  expect(screen.queryByRole('button', {name: '안읽음'})).not.toBeInTheDocument()
  // Embeddings hold received mail only: AI search would answer with inbox mail.
  expect(screen.getByLabelText('검색 방식')).toBeDisabled()
  expect(screen.getByLabelText('검색 방식')).toHaveValue('keyword')
  const paths = vi.mocked(api).mock.calls.map(([path]) => String(path))
  const list = paths.find(path => path.startsWith('/api/messages?'))
  expect(list).toBeDefined()
  expect(list).toContain('folder=sent')
  expect(list).toContain('awaiting_reply=true')
  expect(paths.some(path => /semantic|hybrid/.test(path))).toBe(false)
})
