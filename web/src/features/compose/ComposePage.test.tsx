import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { createMemoryRouter, Link, RouterProvider } from 'react-router-dom'
import { api } from '@/api/client'
import { ComposePage } from './ComposePage'
import type { DraftView, SendPreview } from '../messages/types'

vi.mock('@/api/client', () => ({ api: vi.fn() }))
vi.mock('./RichEditor', () => ({ RichEditor: ({ value, onChange, disabled }: { value: string; onChange: (html: string, text: string) => void; disabled: boolean }) => <textarea aria-label="메일 본문 서식 편집기" disabled={disabled} value={value} onChange={event => onChange(event.target.value, '본문')} /> }))
vi.mock('sonner', () => ({ toast: { success: vi.fn(), error: vi.fn() } }))
const mockAPI = vi.mocked(api)
const view: DraftView = { draft: { id: 'd1', account_id: 'a1', status: 'open', current_version: 1, updated_at: 1 }, version: { version: 1, subject: '공지', to: [{ email: 'hong@corp.local' }], body_text: '본문', body_html: '<h2>본문</h2>', author: 'user' } }
const preview: SendPreview = { draft_id: 'd1', draft_version: 1, from: 'me@corp.local', to: ['hong@corp.local'], subject: '공지', body: '본문', body_html: '<h2>본문</h2>', recipient_count: 1, payload_hash: 'approved-payload' }
let version: DraftView
let currentPreview: SendPreview
let changedApproval = false
let failedSend = false
function setup() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } })
  const router = createMemoryRouter([{ path: '/drafts/:id', element: <><Link to="/sent">테스트 메뉴 이동</Link><ComposePage /></> }, { path: '/sent', element: <div>발송 기록 화면</div> }], { initialEntries: ['/drafts/d1'] })
  return render(<QueryClientProvider client={client}><RouterProvider router={router} /></QueryClientProvider>)
}
beforeEach(() => {
  version = structuredClone(view); currentPreview = structuredClone(preview); changedApproval = false; failedSend = false
  mockAPI.mockReset()
  mockAPI.mockImplementation(async (path, options) => {
    if (path === '/api/accounts') return [{ id: 'a1', email: 'me@corp.local', name: '회사 메일', status: 'active' }]
    if (path === '/api/signatures') return []
    if (path === '/api/preferences') return { fields: [], revision: '' }
    if (path === '/api/mail/templates') return [{ id: 'clean', name: '기본', description: '' }, { id: 'notice', name: '공지', description: '' }]
    if (path === '/api/accounts/a1/preferences') return { fields: [], revision: '' }
    if (path === '/api/mail/render') return { body_html: '<div data-postra-template="notice"><p>공통 렌더 결과</p></div>', body_text: '공통 렌더 결과', format: 'html', template: 'notice' }
    if (path === '/api/drafts/d1/attachments' && options?.method === 'POST') {
      const body = options.body as { name: string }
      version = { ...version, version: { ...version.version, version: version.version.version + 1, attachments: [{ id: 'att1', name: body.name, mime_type: 'text/plain', size: 4, scan_status: 'clean', inline: false }] } }
      return version
    }
    if (path === '/api/drafts/d1' && options?.method === 'PATCH') {
      const body = options.body as { subject: string; body_html: string; body: string; bcc: string[] }
      version = { ...version, version: { ...version.version, version: version.version.version + 1, subject: body.subject, body_html: body.body_html, body_text: body.body } }
      currentPreview = { ...currentPreview, draft_version: version.version.version, subject: body.subject, body_html: body.body_html, payload_hash: 'edited-payload' }
      return version
    }
    if (path === '/api/drafts/d1') return version
    if (path.endsWith('/preview')) return currentPreview
    if (path.endsWith('/request-approval')) return { preview: changedApproval ? { ...currentPreview, payload_hash: 'concurrent-change' } : currentPreview, approval: { token: 'memory-only-token', expires: Math.floor(Date.now() / 1000) + 600 } }
    if (path.endsWith('/send')) { if (failedSend) throw new Error('연결이 끊어졌습니다'); return { id: 'out1', status: 'sent' } }
    throw new Error(`Unexpected test request ${path}`)
  })
})
afterEach(() => { cleanup(); vi.restoreAllMocks() })

describe('explicit draft approval workflow', () => {
  it('uses the shared local renderer without approving or sending, then leaves the result dirty', async () => {
    setup(); await screen.findByDisplayValue('공지')
    fireEvent.change(screen.getByRole('combobox', { name: '메일 템플릿' }), { target: { value: 'notice' } })
    fireEvent.click(screen.getByRole('button', { name: '서식 적용' }))
    await waitFor(() => expect(screen.getByRole('textbox', { name: '메일 본문 서식 편집기' })).toHaveValue('<div data-postra-template="notice"><p>공통 렌더 결과</p></div>'))
    expect(screen.getByText('저장하지 않은 변경')).toBeInTheDocument()
    expect(mockAPI.mock.calls.some(([path]) => path.endsWith('/request-approval') || path.endsWith('/send'))).toBe(false)
  })

  it('uploads a file into a new server version without issuing approval or sending', async () => {
    setup(); await screen.findByDisplayValue('공지')
    fireEvent.change(screen.getByLabelText('첨부파일 선택'), { target: { files: [new File(['test'], 'report.txt', { type: 'text/plain' })] } })
    await screen.findByRole('link', { name: 'report.txt' })
    expect(mockAPI).toHaveBeenCalledWith('/api/drafts/d1/attachments', { method: 'POST', body: { name: 'report.txt', data_base64: 'dGVzdA==', inline: false } })
    expect(screen.getByText('버전 2 · 저장됨')).toBeInTheDocument()
    expect(mockAPI.mock.calls.some(([path]) => path.endsWith('/request-approval') || path.endsWith('/send'))).toBe(false)
  })

  it('does not issue approval or send when showing preview, then sends only after two explicit actions', async () => {
    setup()
    await screen.findByDisplayValue('공지')
    fireEvent.click(screen.getByRole('button', { name: '미리보기 · 발송' }))
    await screen.findByRole('heading', { name: '발송 전 최종 확인' })
    expect(mockAPI.mock.calls.some(([path]) => path.endsWith('/request-approval') || path.endsWith('/send'))).toBe(false)
    const frame = screen.getByTitle('서식 메일 미리보기')
    expect(frame).toHaveAttribute('sandbox', '')
    expect(frame.getAttribute('srcdoc')).toContain("default-src 'none'")
    fireEvent.click(screen.getByRole('button', { name: '내용 확인 · 승인' }))
    await screen.findByRole('button', { name: '승인한 메일 발송' })
    expect(mockAPI.mock.calls.some(([path]) => path.endsWith('/send'))).toBe(false)
    fireEvent.click(screen.getByRole('button', { name: '승인한 메일 발송' }))
    await screen.findByRole('heading', { name: '메일을 발송했습니다' })
    expect(mockAPI).toHaveBeenCalledWith('/api/drafts/d1/send', { method: 'POST', body: { approval_token: 'memory-only-token', idempotency_key: 'draft:d1:v1' } })
  })

  it('discards client approval after editing and requires a new preview', async () => {
    setup(); await screen.findByDisplayValue('공지')
    fireEvent.click(screen.getByRole('button', { name: '미리보기 · 발송' }))
    fireEvent.click(await screen.findByRole('button', { name: '내용 확인 · 승인' }))
    await screen.findByRole('button', { name: '승인한 메일 발송' })
    fireEvent.click(screen.getByRole('button', { name: '미리보기 닫기' }))
    fireEvent.change(screen.getByRole('textbox', { name: '메일 제목' }), { target: { value: '수정된 공지' } })
    fireEvent.click(screen.getByRole('button', { name: '미리보기 · 발송' }))
    await screen.findByRole('button', { name: '내용 확인 · 승인' })
    expect(screen.queryByRole('button', { name: '승인한 메일 발송' })).not.toBeInTheDocument()
    expect(mockAPI.mock.calls.some(([path, options]) => path === '/api/drafts/d1' && options?.method === 'PATCH')).toBe(true)
  })

  it('will not approve a concurrently changed payload', async () => {
    changedApproval = true
    setup(); await screen.findByDisplayValue('공지')
    fireEvent.click(screen.getByRole('button', { name: '미리보기 · 발송' }))
    fireEvent.click(await screen.findByRole('button', { name: '내용 확인 · 승인' }))
    await screen.findByText(/다른 화면에서 초안이 변경되었습니다/)
    expect(screen.queryByRole('button', { name: '승인한 메일 발송' })).not.toBeInTheDocument()
    expect(mockAPI.mock.calls.some(([path]) => path.endsWith('/send'))).toBe(false)
  })

  it('blocks approval when the DLP policy blocks the preview', async () => {
    currentPreview.dlp_blocked = true
    setup(); await screen.findByDisplayValue('공지')
    fireEvent.click(screen.getByRole('button', { name: '미리보기 · 발송' }))
    expect(await screen.findByRole('button', { name: '내용 확인 · 승인' })).toBeDisabled()
    expect(mockAPI.mock.calls.some(([path]) => path.endsWith('/request-approval'))).toBe(false)
  })

  it('does not retry an ambiguous send and directs the user to delivery history', async () => {
    failedSend = true
    setup(); await screen.findByDisplayValue('공지')
    fireEvent.click(screen.getByRole('button', { name: '미리보기 · 발송' }))
    fireEvent.click(await screen.findByRole('button', { name: '내용 확인 · 승인' }))
    fireEvent.click(await screen.findByRole('button', { name: '승인한 메일 발송' }))
    await screen.findByText(/발송 요청의 처리 여부를 확정할 수 없습니다/)
    await waitFor(() => expect(mockAPI.mock.calls.filter(([path]) => path.endsWith('/send'))).toHaveLength(1))
    expect(screen.getByRole('button', { name: '내용 확인 · 승인' })).toBeDisabled()
  })

  it('protects unsaved HTML from internal navigation until discard is explicit', async () => {
    setup(); await screen.findByDisplayValue('공지')
    fireEvent.change(screen.getByRole('textbox', { name: '메일 본문 서식 편집기' }), { target: { value: '<h2>저장하지 않은 내용</h2>' } })
    fireEvent.click(screen.getByRole('link', { name: '테스트 메뉴 이동' }))
    await screen.findByRole('heading', { name: '저장하지 않은 초안이 있습니다' })
    fireEvent.click(screen.getByRole('button', { name: '계속 작성' }))
    expect(screen.getByDisplayValue('<h2>저장하지 않은 내용</h2>')).toBeInTheDocument()
    fireEvent.click(screen.getByRole('link', { name: '테스트 메뉴 이동' }))
    fireEvent.click(await screen.findByRole('button', { name: '변경을 버리고 이동' }))
    await screen.findByText('발송 기록 화면')
    expect(mockAPI.mock.calls.some(([path, options]) => path.endsWith('/send') || options?.method === 'PATCH')).toBe(false)
  })

  it('cancels logout before the session is revoked when unsaved changes are kept', async () => {
    setup(); await screen.findByDisplayValue('공지')
    fireEvent.change(screen.getByRole('textbox', { name: '메일 제목' }), { target: { value: '저장하지 않은 제목' } })
    const confirm = vi.spyOn(window, 'confirm').mockReturnValue(false)
    const event = new Event('postra:before-logout', { cancelable: true })
    expect(fireEvent(window, event)).toBe(false)
    expect(event.defaultPrevented).toBe(true)
    expect(confirm).toHaveBeenCalledOnce()
    expect(screen.getByDisplayValue('저장하지 않은 제목')).toBeInTheDocument()
    expect(mockAPI.mock.calls.some(([path]) => path.endsWith('/logout'))).toBe(false)
  })

  it('restores unsaved protection if a confirmed logout request fails', async () => {
    setup(); await screen.findByDisplayValue('공지')
    fireEvent.change(screen.getByRole('textbox', { name: '메일 제목' }), { target: { value: '계속 보호할 제목' } })
    const confirm = vi.spyOn(window, 'confirm').mockReturnValue(true)
    expect(fireEvent(window, new Event('postra:before-logout', { cancelable: true }))).toBe(true)
    fireEvent(window, new Event('postra:logout-failed'))
    confirm.mockReturnValue(false)
    expect(fireEvent(window, new Event('postra:before-logout', { cancelable: true }))).toBe(false)
    expect(screen.getByDisplayValue('계속 보호할 제목')).toBeInTheDocument()
  })
})
