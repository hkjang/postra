import {cleanup, render, screen, waitFor} from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import {QueryClient, QueryClientProvider} from '@tanstack/react-query'
import {MemoryRouter} from 'react-router-dom'
import {afterEach, beforeEach, describe, expect, it, vi} from 'vitest'
import {api} from '@/api/client'
import {SessionContext, type Principal} from '@/app/session'
import {SettingsEditor} from '@/features/settings/SettingsEditor'
import {MCPAdminPage, MCPKeysPage} from './index'
import {MCPConnectionPanel, parseMCPConnection} from './ConnectionPanel'

vi.mock('@/api/client', () => ({api: vi.fn()}))
vi.mock('sonner', () => ({toast: {success: vi.fn(), error: vi.fn()}}))
const mockedAPI = vi.mocked(api)
const user: Principal = {user_id: 'u1', login_id: 'hong', display_name: '홍길동', role: 'user', auth_method: 'oidc'}
const admin: Principal = {...user, role: 'admin'}
const initial = {
  active_endpoint: '/mcp', configured_endpoint: '/mcp', pending_restart: false,
  oauth: {enabled: true, configured: true, issuer: 'https://keycloak.corp.test/realms/mail', resource_url: 'https://gateway.corp.test/tools/mail', metadata_url: 'https://gateway.corp.test/.well-known/oauth-protected-resource/tools/mail', allowed_client_ids: ['registered-agent'], scopes_supported: ['mail.read', 'mail.search']},
}
const capabilities = {enabled: true, http_enabled: true, endpoint: '/mcp', permissions: {'mail.read': true}, groups: {Discovery: ['mail_capabilities']}, aliases: {}, request_timeout_sec: 120}
let connection: typeof initial
beforeEach(() => {
  connection = structuredClone(initial)
  mockedAPI.mockReset()
  mockedAPI.mockImplementation(async path => {
    if (path === '/api/mcp/connection') return connection
    if (path === '/api/admin/mcp') return capabilities
    return {keys: []}
  })
})
afterEach(() => {cleanup(); vi.restoreAllMocks()})
function mount(node = <MCPConnectionPanel/>, principal = user) {
  const cache = new QueryClient({defaultOptions: {queries: {retry: false}, mutations: {retry: false}}})
  const result = render(<QueryClientProvider client={cache}><SessionContext.Provider value={principal}><MemoryRouter>{node}</MemoryRouter></SessionContext.Provider></QueryClientProvider>)
  return {...result, cache}
}

describe('MCP OAuth connection guidance', () => {
  it('shows server canonical URLs, first-login and separate client callback guidance without storing or requesting tokens', async () => {
    const person = userEvent.setup()
    const storage = vi.spyOn(Storage.prototype, 'setItem')
    const clipboard = vi.spyOn(navigator.clipboard, 'writeText')
    mount()
    expect(await screen.findByText('OAuth 설정됨 · 연결 미확인')).toBeInTheDocument()
    expect(screen.getByText(/먼저 Postra에 SSO로 한 번 로그인/)).toBeInTheDocument()
    expect(screen.getByText(/Authorization Code \+ PKCE S256/)).toHaveTextContent('Postra 웹 SSO Callback URL과는 다릅니다')
    expect(screen.getByText('registered-agent')).toBeInTheDocument()
    expect(screen.getByText('mail.read')).toBeInTheDocument()
    const metadata = screen.getByRole('link', {name: initial.oauth.metadata_url})
    expect(metadata).toHaveAttribute('href', initial.oauth.metadata_url)
    expect(metadata).toHaveAttribute('rel', 'noopener noreferrer')
    await person.click(screen.getByRole('button', {name: 'Resource URL 복사'}))
    expect(clipboard).toHaveBeenCalledWith(initial.oauth.resource_url)
    expect(storage).not.toHaveBeenCalled()
    expect(screen.queryByRole('textbox')).not.toBeInTheDocument()
    expect(screen.queryByRole('link', {name: '관리자 MCP 설정 열기'})).not.toBeInTheDocument()
    expect(mockedAPI.mock.calls.every(([path, options]) => path === '/api/mcp/connection' && !options?.method)).toBe(true)
  })

  it('keeps personal API-key provisioning when OAuth is off and never invents an absolute endpoint', async () => {
    connection.oauth = {...connection.oauth, enabled: false, configured: false, resource_url: '', metadata_url: '', issuer: ''}
    mount(<MCPKeysPage/>)
    expect(await screen.findByText('OAuth 사용 안 함')).toBeInTheDocument()
    expect(await screen.findByLabelText('클라이언트·키 이름')).toBeInTheDocument()
    expect(screen.getByRole('button', {name: '키 발급'})).toBeInTheDocument()
    expect(screen.getByText('미설정 · 관리자에게 연결 주소를 확인하세요.')).toBeInTheDocument()
    expect(screen.queryByRole('button', {name: 'Resource URL 복사'})).not.toBeInTheDocument()
    expect(mockedAPI.mock.calls.some(([path]) => path.startsWith('/api/admin/'))).toBe(false)
  })

  it('distinguishes incomplete OAuth configuration and the active endpoint from restart-pending configuration', async () => {
    connection = {...connection, configured_endpoint: '/agent/mail', pending_restart: true, oauth: {...connection.oauth, configured: false}}
    mount(undefined, admin)
    expect(await screen.findByText('OAuth 설정 필요')).toBeInTheDocument()
    expect(screen.getByText('Endpoint 재시작 대기')).toBeInTheDocument()
    expect(screen.getByText('현재 실행 Endpoint').nextElementSibling).toHaveTextContent('/mcp')
    expect(screen.getByText('재시작 후 Endpoint').nextElementSibling).toHaveTextContent('/agent/mail')
    expect(screen.getByRole('link', {name: '관리자 MCP 설정 열기'})).toHaveAttribute('href', '/admin?category=mcp')
  })

  it('normalizes only known nil-list representations without granting defaults', async () => {
    mockedAPI.mockResolvedValue({...connection, oauth: {...connection.oauth, allowed_client_ids: null, scopes_supported: null}})
    mount()
    expect(await screen.findByText('허용된 클라이언트 없음')).toBeInTheDocument()
    expect(screen.getByText('허용된 권한 없음')).toBeInTheDocument()
  })

  it.each([
    null, {}, {...initial, oauth: null}, {...initial, oauth: {...initial.oauth, scopes_supported: [null]}},
    {...initial, oauth: {...initial.oauth, allowed_client_ids: {client: 'do-not-echo-private-payload'}}},
    {...initial, active_endpoint: '//untrusted.test'},
    {...initial, oauth: {...initial.oauth, metadata_url: 'javascript:do-not-echo-private-payload'}},
    {...initial, oauth: {...initial.oauth, issuer: 'https://user:do-not-echo-private-payload@provider.test'}},
    {...initial, oauth: {...initial.oauth, resource_url: 'https://gateway.test/mcp?token=do-not-echo-private-payload'}},
  ])('fails safely for malformed or credential-bearing discovery payload %#', async response => {
    mockedAPI.mockResolvedValue(response); mount()
    expect(await screen.findByRole('alert')).toHaveTextContent('서버 응답 형식')
    expect(document.body).not.toHaveTextContent('do-not-echo-private-payload')
    expect(screen.queryByRole('button', {name: 'Resource URL 복사'})).not.toBeInTheDocument()
  })

  it('drops unknown credential fields instead of exposing or caching them in parsed query data', () => {
    const result = parseMCPConnection({...initial, access_token: 'sensitive', oauth: {...initial.oauth, client_secret: 'sensitive', refresh_token: 'sensitive'}})
    expect(JSON.stringify(result)).not.toContain('sensitive')
  })

  it('does not lose access to existing key management if connection discovery fails', async () => {
    mockedAPI.mockImplementation(async path => {
      if (path === '/api/mcp/connection') throw new Error('연결 정보를 조회하지 못했습니다.')
      return {keys: []}
    })
    mount(<MCPKeysPage/>)
    expect(await screen.findByRole('alert')).toHaveTextContent('연결 정보를 조회하지 못했습니다.')
    expect(await screen.findByLabelText('클라이언트·키 이름')).toBeInTheDocument()
    expect(screen.getByRole('button', {name: '키 발급'})).toBeInTheDocument()
  })

  it('shows OAuth alongside administrator key inventory while rejecting malformed capability maps safely', async () => {
    mockedAPI.mockImplementation(async path => path === '/api/mcp/connection' ? connection : path === '/api/admin/mcp' ? {...capabilities, groups: {Read: {secret: 'do-not-echo-private-payload'}}} : {keys: []})
    mount(<MCPAdminPage/>, admin)
    expect(await screen.findByText('OAuth 설정됨 · 연결 미확인')).toBeInTheDocument()
    expect(await screen.findByRole('alert')).toHaveTextContent('서버 응답 형식')
    expect(screen.getByRole('heading', {name: '전체 사용자 MCP 키'})).toBeInTheDocument()
    expect(document.body).not.toHaveTextContent('do-not-echo-private-payload')
  })

  it('refreshes both OAuth connection information and MCP capabilities after administrator settings save', async () => {
    let saved = false
    const settings = () => ({revision: saved ? 'r2' : 'r1', fields: [{key: 'mcp.oauth.enabled', label: 'MCP OAuth 활성화', category: 'mcp', type: 'bool', value: String(saved), default: 'false', scope: 'admin', apply: 'live', secret: false, source: 'admin', locked: false, lockable: false}]})
    mockedAPI.mockImplementation(async (path, options) => {
      if (path === '/api/accounts' || path === '/api/signatures') return []
      if (path === '/api/admin/configuration') {if (options?.method === 'PATCH') saved = true; return settings()}
      if (path === '/api/mcp/connection') return {...connection, oauth: {...connection.oauth, enabled: saved}}
      if (path === '/api/admin/mcp') return capabilities
      return {keys: []}
    })
    const person = userEvent.setup()
    mount(<><SettingsEditor admin category="mcp"/><MCPAdminPage/></>, admin)
    await screen.findByText('OAuth 사용 안 함')
    await person.click(await screen.findByLabelText('MCP OAuth 활성화'))
    await person.click(screen.getByRole('button', {name: '변경 확인'}))
    await person.click(screen.getByRole('button', {name: '확인 후 저장'}))
    expect(await screen.findByText('OAuth 설정됨 · 연결 미확인')).toBeInTheDocument()
    await waitFor(() => expect(mockedAPI.mock.calls.filter(([path]) => path === '/api/admin/mcp').length).toBe(2))
    expect(mockedAPI.mock.calls.filter(([path]) => path === '/api/mcp/connection').length).toBe(2)
  })
})
