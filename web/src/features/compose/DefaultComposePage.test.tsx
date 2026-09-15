import { afterEach, beforeEach, expect, it, vi } from 'vitest'
import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react'
import { createMemoryRouter, RouterProvider } from 'react-router-dom'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { api } from '@/api/client'
import { ComposePage } from './ComposePage'

vi.mock('@/api/client', () => ({ api: vi.fn() }))
vi.mock('./RichEditor', () => ({ RichEditor: ({ value, onChange }: { value: string; onChange: (html: string, text: string) => void }) => <textarea aria-label="작성 본문" value={value} onChange={event => onChange(event.target.value, '본문')}/> }))
const accounts = ['first', 'preferred'].map(id => ({ id, email: `${id}@corp.local`, name: id, status: 'active' }))
const mockAPI = vi.mocked(api)
beforeEach(() => {
  mockAPI.mockReset(); mockAPI.mockImplementation(async (path, options) => {
    if (path === '/api/accounts') return accounts
    if (path === '/api/preferences') return { fields: [{ key: 'mail.default_account_id', value: 'preferred' }], revision: '' }
    if (path.endsWith('/preferences')) return { fields: [{ key: 'compose.format', value: 'html' }], revision: '' }
    if (path === '/api/signatures' || path === '/api/mail/templates') return []
    if (path === '/api/drafts' && options?.method === 'POST') {
      const body = options.body as { account_id: string; bcc: string[] }
      return { draft: { id: 'new-draft', account_id: body.account_id, status: 'open' }, version: { version: 1, subject: '공지', body_text: '본문', body_html: '<p>본문</p>', to: [], bcc: body.bcc.map(email => ({ email })), author: 'user' } }
    }
    throw new Error(`Unexpected request ${path}`)
  })
})
afterEach(() => { cleanup(); vi.restoreAllMocks() })
function mount(path = '/compose') {
  const router = createMemoryRouter([{ path: '/compose', element: <ComposePage/> }, { path: '/drafts/:id', element: <p>저장 완료 화면</p> }], { initialEntries: [path] })
  render(<QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}><RouterProvider router={router}/></QueryClientProvider>)
}
it('hydrates the owned active personal default, keeps later manual choices, and creates Bcc atomically', async () => {
  mount(); await waitFor(() => expect(screen.getByRole('combobox', { name: '발신 계정' })).toHaveValue('preferred'))
  fireEvent.change(screen.getByRole('combobox', { name: '발신 계정' }), { target: { value: 'first' } })
  expect(screen.getByRole('combobox', { name: '발신 계정' })).toHaveValue('first')
  fireEvent.change(screen.getByLabelText('숨은참조'), { target: { value: 'private@corp.local' } })
  fireEvent.change(screen.getByLabelText('메일 제목'), { target: { value: '공지' } })
  fireEvent.click(screen.getByRole('button', { name: '초안 저장' })); await screen.findByText('저장 완료 화면')
  expect(mockAPI.mock.calls.filter(([path, options]) => path === '/api/drafts' && options?.method === 'POST')).toEqual([['/api/drafts', expect.objectContaining({ body: expect.objectContaining({ account_id: 'first', bcc: ['private@corp.local'] }) })]])
  expect(mockAPI.mock.calls.some(([, options]) => options?.method === 'PATCH')).toBe(false)
})
it('honors a valid explicit account before the personal default', async () => {
  mount('/compose?account=first'); await waitFor(() => expect(screen.getByRole('combobox', { name: '발신 계정' })).toHaveValue('first'))
  expect(mockAPI).toHaveBeenCalledWith('/api/accounts/first/preferences', expect.objectContaining({ signal: expect.any(AbortSignal) }))
})
