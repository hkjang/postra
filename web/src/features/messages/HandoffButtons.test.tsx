import { afterEach, expect, it, vi } from 'vitest'
import { cleanup, render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { api } from '@/api/client'
import { HandoffButtons, handoffURL } from './HandoffButtons'

vi.mock('@/api/client', () => ({ api: vi.fn() }))
vi.mock('sonner', () => ({ toast: { success: vi.fn(), error: vi.fn() } }))
const mockAPI = vi.mocked(api)
const claim = { claim: 'c_-1', source: 'https://postra.intra', filename: '분기 보고.md', content_type: 'text/markdown; charset=utf-8', bytes: 42, expires_at: '2026-09-16T07:05:00+09:00' }
afterEach(() => { cleanup(); vi.restoreAllMocks(); vi.unstubAllGlobals() })

function renderButtons() {
  return render(<QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}><HandoffButtons messageID="msg_1"/></QueryClientProvider>)
}

it('opens the receiving service at the standard /handoff address', () => {
  expect(handoffURL({ name: 'Ptium', origin: 'https://ptium.intra' }, claim)).toBe('https://ptium.intra/handoff?source=https%3A%2F%2Fpostra.intra&claim=c_-1')
})

it('shows nothing while the allow list is empty and never asks for a claim', async () => {
  mockAPI.mockResolvedValue({ format: 'markdown', targets: [] })
  renderButtons()
  await waitFor(() => expect(mockAPI).toHaveBeenCalledWith('/api/handoff/targets', expect.anything()))
  expect(screen.queryByRole('button')).toBeNull()
  expect(mockAPI).not.toHaveBeenCalledWith('/api/handoff/claims', expect.anything())
})

it('asks for a claim only on click, opens the window first and points it at the target', async () => {
  const user = userEvent.setup()
  const win = { opener: {}, location: { replace: vi.fn() }, close: vi.fn() }
  const open = vi.spyOn(window, 'open').mockReturnValue(win as unknown as Window)
  mockAPI.mockImplementation(async (path: string) => path === '/api/handoff/targets' ? { format: 'markdown', targets: [{ name: 'Ptium', origin: 'https://ptium.intra' }] } : claim)
  renderButtons()
  const button = await screen.findByRole('button', { name: 'Ptium' })
  expect(mockAPI).not.toHaveBeenCalledWith('/api/handoff/claims', expect.anything())
  await user.click(button)
  await waitFor(() => expect(win.location.replace).toHaveBeenCalledWith('https://ptium.intra/handoff?source=https%3A%2F%2Fpostra.intra&claim=c_-1'))
  expect(open).toHaveBeenCalledWith('about:blank', '_blank')
  expect(win.opener).toBeNull()
  expect(mockAPI).toHaveBeenCalledWith('/api/handoff/claims', { method: 'POST', body: { resource: 'msg_1', format: 'markdown' } })
  expect(win.close).not.toHaveBeenCalled()
})

it('closes the window again when no claim could be issued', async () => {
  const user = userEvent.setup()
  const win = { opener: {}, location: { replace: vi.fn() }, close: vi.fn() }
  vi.spyOn(window, 'open').mockReturnValue(win as unknown as Window)
  mockAPI.mockImplementation(async (path: string) => { if (path === '/api/handoff/targets') return { format: 'markdown', targets: [{ name: 'Ptium', origin: 'https://ptium.intra' }] }; throw new Error('본문을 읽을 수 없어 보낼 수 없습니다') })
  renderButtons()
  await user.click(await screen.findByRole('button', { name: 'Ptium' }))
  await waitFor(() => expect(win.close).toHaveBeenCalled())
  expect(win.location.replace).not.toHaveBeenCalled()
})
