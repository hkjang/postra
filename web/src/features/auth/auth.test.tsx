import {afterEach, beforeEach, expect, it, vi} from 'vitest'
import {cleanup, render, screen, waitFor} from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import {QueryClient, QueryClientProvider} from '@tanstack/react-query'
import {MemoryRouter} from 'react-router-dom'
import {api} from '@/api/client'
import {LoginPage, SetupPage, type BrowserSession} from './index'

vi.mock('@/api/client', () => ({api: vi.fn()}))
const mockAPI = vi.mocked(api)
const session: BrowserSession = {authenticated: false, auth_enabled: true, local_auth: true, login_url: '/app/login'}
beforeEach(() => {mockAPI.mockReset(); localStorage.clear(); sessionStorage.clear()})
afterEach(() => {cleanup(); vi.restoreAllMocks()})
function mount(element: React.ReactNode) {
  const cache = new QueryClient({defaultOptions: {queries: {retry: false}}})
  render(<QueryClientProvider client={cache}><MemoryRouter>{element}</MemoryRouter></QueryClientProvider>)
  return cache
}

it('submits local credentials only through the new JSON bridge and keeps a deep return target', async () => {
  const user = userEvent.setup()
  mockAPI.mockRejectedValue(new Error('로그인 ID 또는 비밀번호가 올바르지 않습니다.'))
  const store = vi.spyOn(Storage.prototype, 'setItem')
  mount(<LoginPage session={session} returnTo="/app/messages/own-message?q=review"/>)
  await user.type(screen.getByLabelText('로그인 ID'), 'fixture-user')
  await user.type(screen.getByLabelText('비밀번호'), 'fixture-secret')
  await user.click(screen.getByRole('button', {name: '계정으로 로그인'}))
  await screen.findByText('로그인 ID 또는 비밀번호가 올바르지 않습니다.')
  expect(mockAPI).toHaveBeenCalledWith('/auth/login', {body: {login_id: 'fixture-user', password: 'fixture-secret', return_to: '/app/messages/own-message?q=review'}})
  expect(store).not.toHaveBeenCalled()
  expect(screen.getByRole('button', {name: '계정으로 로그인'})).toBeEnabled()
})

it('shows a token-only form without persisting an API credential', async () => {
  const user = userEvent.setup()
  mockAPI.mockRejectedValue(new Error('토큰이 올바르지 않습니다.'))
  const store = vi.spyOn(Storage.prototype, 'setItem')
  mount(<LoginPage session={{...session, local_auth: false, token_required: true}} returnTo="/app/"/>)
  expect(screen.queryByLabelText('로그인 ID')).not.toBeInTheDocument()
  await user.type(screen.getByLabelText('접속 토큰'), 'fixture-token')
  await user.click(screen.getByRole('button', {name: '계정으로 로그인'}))
  await screen.findByText('토큰이 올바르지 않습니다.')
  expect(mockAPI).toHaveBeenCalledWith('/auth/login', {body: {token: 'fixture-token', return_to: '/app/'}})
  expect(store).not.toHaveBeenCalled()
})

it('uses the standalone SSO route, escapes signed error text and gates bootstrap', () => {
  const view = mount(<LoginPage session={{...session, oidc_url: '/auth/oidc/start?return_to=%2Fapp%2F', sso_error: '<script>unsafe()</script>'}} returnTo="/app/mail?q=contract"/>)
  expect(screen.getByRole('link', {name: '회사 SSO로 로그인'})).toHaveAttribute('href', '/auth/oidc/start?return_to=%2Fapp%2Fmail%3Fq%3Dcontract')
  expect(screen.getByText('<script>unsafe()</script>')).toBeInTheDocument()
  expect(document.querySelector('script')).toBeNull()
  view.clear(); cleanup()
  mount(<LoginPage session={{...session, setup_url: '/app/setup'}} returnTo="/app/"/>)
  expect(screen.getByRole('link', {name: '최초 관리자 설정'})).toHaveAttribute('href', '/setup')
  expect(screen.queryByLabelText('로그인 ID')).not.toBeInTheDocument()
})

it.each([{required: false, allowed: false}, {required: true, allowed: false}])('never renders a bootstrap form when state rejects it: %s', async state => {
  mockAPI.mockResolvedValue({...state, login_url: '/app/login'})
  mount(<SetupPage/>)
  await waitFor(() => expect(screen.queryByRole('status')).not.toBeInTheDocument())
  expect(mockAPI).toHaveBeenCalledWith('/auth/setup')
  expect(screen.queryByRole('button', {name: '관리자 계정 만들기'})).not.toBeInTheDocument()
})

it('requires matching bootstrap passwords before issuing a write', async () => {
  const user = userEvent.setup()
  mockAPI.mockResolvedValue({required: true, allowed: true, login_url: '/app/login'})
  mount(<SetupPage/>)
  await user.type(await screen.findByLabelText('로그인 ID'), 'fixture-admin')
  await user.type(screen.getByLabelText('표시 이름'), '관리자')
  const passwords = screen.getAllByLabelText(/^비밀번호/)
  await user.type(passwords[0], 'fixture-password-2026')
  await user.type(passwords[1], 'mismatched-password')
  await user.click(screen.getByRole('button', {name: '관리자 계정 만들기'}))
  await screen.findByText('비밀번호 확인이 일치하지 않습니다.')
  expect(mockAPI).toHaveBeenCalledTimes(1)
})
