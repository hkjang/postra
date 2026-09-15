import { afterEach, beforeEach, expect, it, vi } from 'vitest'
import { act, cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { createMemoryRouter, RouterProvider } from 'react-router-dom'
import { api } from '@/api/client'
import { SignaturesPage } from './SignaturesPage'

vi.mock('@/api/client', () => ({ api: vi.fn() }))
vi.mock('./RichEditor', () => ({ RichEditor: ({ onChange }: { onChange: (html: string, text: string) => void }) => <button type="button" onClick={() => onChange('<p style="line-height: 1.7"><br></p>', '')}>본문 비우기</button> }))
const mockAPI = vi.mocked(api)
beforeEach(() => {
  mockAPI.mockReset(); mockAPI.mockImplementation(async (path, options) => {
    if (path === '/api/accounts') return []
    if (path === '/api/signatures' && options?.method === 'POST') return { id: 'sig_own', name: '기본', body_html: '<p>홍길동</p>', body_text: '홍길동' }
    if (path === '/api/signatures') return []
    throw new Error('Unexpected request')
  })
})
afterEach(() => { cleanup(); vi.restoreAllMocks() })
function mount() {
  const router = createMemoryRouter([{ path: '/settings/signatures', element: <SignaturesPage/> }], { initialEntries: ['/settings/signatures'] })
  return render(<QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}><RouterProvider router={router}/></QueryClientProvider>)
}
it('sends a truly empty body for structured identity generation after a styled editor is cleared', async () => {
  mount(); await screen.findByLabelText('서명 이름')
  fireEvent.change(screen.getByLabelText('서명 이름'), { target: { value: '기본' } }); fireEvent.change(screen.getByLabelText('서명 표시 이름'), { target: { value: '홍길동' } })
  fireEvent.click(screen.getByRole('button', { name: '본문 비우기' })); fireEvent.click(screen.getByRole('button', { name: '서명 저장' }))
  await waitFor(() => expect(mockAPI.mock.calls.filter(([, options]) => options?.method === 'POST')).toHaveLength(1))
  expect(mockAPI).toHaveBeenCalledWith('/api/signatures', expect.objectContaining({ method: 'POST', body: expect.objectContaining({ body_html: '', format: 'html' }) }))
})
it('keeps the unsaved guard after canceled or failed logout and avoids duplicate unload confirmation on acceptance', async () => {
  mount(); await screen.findByRole('button', { name: '본문 비우기' }); fireEvent.click(screen.getByRole('button', { name: '본문 비우기' }))
  const confirm = vi.spyOn(window, 'confirm').mockReturnValue(false)
  act(() => { expect(window.dispatchEvent(new Event('postra:before-logout', { cancelable: true }))).toBe(false) })
  expect(screen.getByText('저장하지 않은 변경')).toBeInTheDocument()
  confirm.mockReturnValue(true)
  act(() => { expect(window.dispatchEvent(new Event('postra:before-logout', { cancelable: true }))).toBe(true) })
  const unload = new Event('beforeunload', { cancelable: true }); window.dispatchEvent(unload); expect(unload.defaultPrevented).toBe(false)
  act(() => { window.dispatchEvent(new Event('postra:logout-failed')) }); expect(screen.getByText('저장하지 않은 변경')).toBeInTheDocument()
  const afterFailure = new Event('beforeunload', { cancelable: true }); window.dispatchEvent(afterFailure); expect(afterFailure.defaultPrevented).toBe(true)
})
