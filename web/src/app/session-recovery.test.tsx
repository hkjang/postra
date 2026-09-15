import { useState } from 'react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { act, cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { createMemoryRouter, Outlet, RouterProvider } from 'react-router-dom'
import { api, APIError } from '@/api/client'
import { App } from './App'

vi.mock('@/api/client', async importOriginal => {
  const original = await importOriginal<typeof import('@/api/client')>()
  return { ...original, api: vi.fn() }
})
vi.mock('@/components/layout/Workspace', () => ({ Workspace: () => <Outlet /> }))
vi.mock('@/features/tracking', () => ({BrowserTracking: () => null}))
vi.mock('@/features/inbox/InboxPage', () => ({ InboxPage: function TestEditor() {
  const [text, setText] = useState('')
  return <label>작성 중인 본문<input value={text} onChange={event => setText(event.target.value)} /></label>
} }))

const loggedIn = { authenticated: true, auth_enabled: true, principal: { user_id: 'user-a', login_id: 'a@corp.local', role: 'user', display_name: 'A', auth_method: 'oidc' }, login_url: '/app/login' }
const mockAPI = vi.mocked(api)
beforeEach(() => mockAPI.mockReset())
afterEach(cleanup)

function mount() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  const router = createMemoryRouter([{ path: '*', element: <App /> }], { initialEntries: ['/mail'] })
  render(<QueryClientProvider client={client}><RouterProvider router={router} /></QueryClientProvider>)
  return client
}

describe('session background refresh recovery', () => {
  it.each([new TypeError('Failed to fetch'), new APIError('서버가 재기동 중입니다', 503)])('preserves local editor state on transient refresh failure: %s', async error => {
    mockAPI.mockResolvedValueOnce(loggedIn)
    const client = mount()
    const editor = await screen.findByRole('textbox', { name: '작성 중인 본문' })
    fireEvent.change(editor, { target: { value: '아직 저장하지 않은 중요한 메일' } })

    mockAPI.mockRejectedValueOnce(error)
    await act(async () => { await client.invalidateQueries({ queryKey: ['session'] }) })
    await screen.findByText('서버 연결을 확인하지 못했습니다. 작성 중인 내용은 유지됩니다.')
    expect(screen.getByRole('textbox', { name: '작성 중인 본문' })).toHaveValue('아직 저장하지 않은 중요한 메일')

    mockAPI.mockResolvedValueOnce(loggedIn)
    fireEvent.click(screen.getByRole('button', { name: '연결 다시 확인' }))
    await act(async () => { await client.getQueryCache().find({ queryKey: ['session'] })?.promise })
    expect(screen.getByRole('textbox', { name: '작성 중인 본문' })).toHaveValue('아직 저장하지 않은 중요한 메일')

    mockAPI.mockResolvedValueOnce({ authenticated: false, auth_enabled: true, login_url: '/app/login' })
    await act(async () => { await client.invalidateQueries({ queryKey: ['session'] }) })
    await screen.findByRole('button', { name: '계정으로 로그인' })
    expect(screen.queryByRole('textbox', { name: '작성 중인 본문' })).not.toBeInTheDocument()
    expect(screen.queryByDisplayValue('아직 저장하지 않은 중요한 메일')).not.toBeInTheDocument()
  })

  it('removes private editor state after explicit authentication failure', async () => {
    mockAPI.mockResolvedValueOnce(loggedIn)
    const client = mount()
    fireEvent.change(await screen.findByRole('textbox', { name: '작성 중인 본문' }), { target: { value: '로그아웃 뒤 감출 본문' } })
    mockAPI.mockRejectedValueOnce(new APIError('인증이 만료되었습니다', 401))
    await act(async () => { await client.invalidateQueries({ queryKey: ['session'] }) })
    await screen.findByText('인증이 만료되었습니다')
    expect(screen.queryByRole('textbox', { name: '작성 중인 본문' })).not.toBeInTheDocument()
  })

  it('does not reuse local component state after another identity signs in', async () => {
    mockAPI.mockResolvedValueOnce(loggedIn)
    const client = mount()
    fireEvent.change(await screen.findByRole('textbox', { name: '작성 중인 본문' }), { target: { value: 'A 사용자 본문' } })
    mockAPI.mockResolvedValueOnce({ ...loggedIn, principal: { ...loggedIn.principal, user_id: 'user-b', login_id: 'b@corp.local' } })
    await act(async () => { await client.invalidateQueries({ queryKey: ['session'] }) })
    await waitFor(() => expect(screen.getByRole('textbox', { name: '작성 중인 본문' })).toHaveValue(''))
  })
})
