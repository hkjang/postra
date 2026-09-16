import {afterEach, beforeEach, expect, it, vi} from 'vitest'
import {cleanup, render, screen} from '@testing-library/react'
import {MemoryRouter} from 'react-router-dom'
import {QueryClient, QueryClientProvider} from '@tanstack/react-query'
import {api} from '@/api/client'
import {InboxPage, mailRefreshInterval} from './InboxPage'

const captured = vi.hoisted(() => [] as {refetchInterval?: unknown; refetchIntervalInBackground?: boolean}[])
vi.mock('@tanstack/react-query', async importActual => {
  const actual = await importActual<typeof import('@tanstack/react-query')>()
  return {...actual, useInfiniteQuery: (options: Parameters<typeof actual.useInfiniteQuery>[0]) => {captured.push(options); return actual.useInfiniteQuery(options)}}
})
vi.mock('@/api/client', () => ({api: vi.fn()}))
vi.mock('@/features/settings/usePreferenceBridge', () => ({usePreferenceBridge: () => ({value: (_key: string, fallback: string) => fallback, locked: () => false})}))
vi.mock('./SearchTools', () => ({AdvancedSearch: () => null, BulkActions: () => null, keywordSearchParams: (params: URLSearchParams) => new URLSearchParams({q: params.get('q') || '', folder: params.get('folder') || 'inbox'})}))
vi.mock('../messages/MessagePage', () => ({MessagePane: () => null}))
beforeEach(() => {captured.length = 0; vi.mocked(api).mockReset()})
afterEach(() => cleanup())

it.each([
  ['', 'keyword', 'inbox', 60_000], ['', 'keyword', 'snoozed', 60_000], ['contract', 'keyword', 'inbox', 60_000],
  ['', 'semantic', 'inbox', 60_000], ['', 'hybrid', 'snoozed', 60_000],
  ['contract', 'semantic', 'inbox', false], ['contract', 'hybrid', 'snoozed', false],
  ['', 'keyword', 'archive', false], ['', 'keyword', 'important', false],
] as const)('polls only eligible GET inbox/reminder requests: %s/%s/%s', async (query, mode, folder, interval) => {
  expect(mailRefreshInterval(query, mode, folder)).toBe(interval)
  const aiSearch = Boolean(query && mode !== 'keyword')
  vi.mocked(api).mockResolvedValue(aiSearch ? {results: []} : {messages: []})
  const params = new URLSearchParams({q: query, mode, folder})
  render(<QueryClientProvider client={new QueryClient({defaultOptions: {queries: {retry: false}}})}><MemoryRouter initialEntries={['/mail?' + params]}><InboxPage/></MemoryRouter></QueryClientProvider>)
  await screen.findByRole('heading', {name: query ? '검색 결과가 없습니다' : '아직 받은 메일이 없습니다'})
  expect(captured.at(-1)?.refetchInterval).toBe(interval)
  expect(captured.at(-1)?.refetchIntervalInBackground).toBe(false)
  expect(api).toHaveBeenCalledTimes(1)
  const [path, options] = vi.mocked(api).mock.calls[0]
  expect(path).toMatch(aiSearch ? /\/api\/(semantic|hybrid)-search/ : /^\/api\/messages\?/)
  expect(options?.method).toBe(aiSearch ? 'POST' : undefined)
})
