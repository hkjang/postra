import {cleanup, render, screen} from '@testing-library/react'
import {QueryClient, QueryClientProvider} from '@tanstack/react-query'
import {MemoryRouter} from 'react-router-dom'
import {afterEach, beforeEach, describe, expect, it, vi} from 'vitest'
import {api} from '@/api/client'
import {JobsPage} from './index'

vi.mock('@/api/client', () => ({api: vi.fn()}))
const mockedAPI = vi.mocked(api)

const failed = {
  id: 'job_1', account_id: 'acc_1', type: 'sync', status: 'failed', progress: '0/12',
  error: '메일 서버가 제한 시간 안에 응답하지 않았습니다. 작업 상세에서 멈춘 단계와 소요 시간을 확인하세요.',
  stats: {seen: 12, new: 0, failed: 0},
  diagnostic: {
    stage: 'greeting', class: 'timeout', command: 'RETR', code: 'UNAVAILABLE',
    elapsed_ms: 60000, timeout_ms: 60000, attempts: 3, protocol: 'imap', host: 'mail.corp.local', port: 993,
    host_sessions: 2, account_sessions: 2, slot_wait_ms: 4200,
    summary: 'IMAP mail.corp.local:993 (tls) · 서버 환영 메시지 수신 단계: 제한 시간 안에 응답이 없었습니다 · 제한 60.0초 중 60.0초 경과 · 연결 시도 3회',
  },
  created_at: 1_790_000_000,
}

beforeEach(() => {
  mockedAPI.mockReset()
  mockedAPI.mockImplementation(async () => [failed])
})
afterEach(() => {cleanup(); vi.restoreAllMocks()})

function mount() {
  const cache = new QueryClient({defaultOptions: {queries: {retry: false}}})
  return render(<QueryClientProvider client={cache}><MemoryRouter><JobsPage/></MemoryRouter></QueryClientProvider>)
}

describe('sync failure diagnostics', () => {
  it('shows where the sync stopped, with the numbers an operator can act on', async () => {
    mount()
    expect(await screen.findByText(/서버 환영 메시지 수신 단계/)).toBeInTheDocument()
    const panel = screen.getByRole('group', {name: '동기화 진단'})
    expect(panel).toHaveTextContent('멈춘 지점')
    expect(screen.getByText('서버').nextElementSibling).toHaveTextContent('IMAP mail.corp.local:993')
    expect(screen.getByText('명령').nextElementSibling).toHaveTextContent('RETR')
    expect(screen.getByText('서버 응답 코드').nextElementSibling).toHaveTextContent('UNAVAILABLE')
    expect(screen.getByText('소요 시간').nextElementSibling).toHaveTextContent('60.0초')
    expect(screen.getByText('이 단계의 제한 시간').nextElementSibling).toHaveTextContent('60.0초')
    expect(screen.getByText('연결 시도').nextElementSibling).toHaveTextContent('3회')
    expect(screen.getByText('이 서버에 유지 중인 연결').nextElementSibling).toHaveTextContent('2개 (이 계정 2개)')
    expect(screen.getByText('시작 전 대기').nextElementSibling).toHaveTextContent('4.2초')
    expect(panel).toHaveTextContent('메일 서버가 보낸 문장은 포함하지 않습니다')
  })

  it('renders a partially succeeded sync as its own outcome, not a clean one', async () => {
    mockedAPI.mockResolvedValue([{...failed, status: 'partially_succeeded', stats: {new: 4, failed: 2},
      diagnostic: {...failed.diagnostic, summary: '일부 메일을 받지 못했습니다'}}])
    mount()
    expect(await screen.findByText('일부 완료')).toBeInTheDocument()
    expect(screen.getByText('멈춘 지점')).toBeInTheDocument()
    expect(screen.getByText('신규').nextElementSibling).toHaveTextContent('4')
    expect(screen.getByText('실패').nextElementSibling).toHaveTextContent('2')
  })

  it('keeps the diagnostic optional: a job without one still renders', async () => {
    mockedAPI.mockResolvedValue([{id: 'job_2', type: 'sync', status: 'succeeded', stats: {new: 1}, created_at: 1_790_000_001}])
    mount()
    expect(await screen.findByText('완료')).toBeInTheDocument()
    expect(screen.queryByRole('group', {name: '동기화 진단'})).not.toBeInTheDocument()
  })

  it('labels a successful sync diagnostic as information rather than a failure', async () => {
    mockedAPI.mockResolvedValue([{id: 'job_3', type: 'sync', status: 'succeeded', stats: {new: 1}, created_at: 1_790_000_002,
      diagnostic: {summary: 'IMAP mail.corp.local:993 (tls) · 보낸 메일함도 확인했습니다', host: 'mail.corp.local', protocol: 'imap', port: 993}}])
    mount()
    expect(await screen.findByText('동기화 진단')).toBeInTheDocument()
    expect(screen.queryByText('멈춘 지점')).not.toBeInTheDocument()
    expect(screen.getByText(/보낸 메일함도 확인했습니다/)).toBeInTheDocument()
  })

  it('drops a malformed diagnostic instead of rendering unknown payload', async () => {
    mockedAPI.mockResolvedValue([{...failed, diagnostic: {summary: 12345, host: {evil: 'do-not-echo-private-payload'}}}])
    mount()
    expect(await screen.findByRole('alert')).toHaveTextContent('서버 응답 형식')
    expect(document.body).not.toHaveTextContent('do-not-echo-private-payload')
  })
})
