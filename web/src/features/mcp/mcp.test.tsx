import {cleanup, render, screen, waitFor, within} from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import {QueryClient, QueryClientProvider} from '@tanstack/react-query'
import {afterEach, beforeEach, describe, expect, it, vi} from 'vitest'
import {SessionContext, type Principal} from '@/app/session'
import {api} from '@/api/client'
import {MCPAdminPage, MCPKeysPage} from './index'

vi.mock('@/api/client', () => ({api: vi.fn()}))
vi.mock('sonner', () => ({toast: {success: vi.fn(), error: vi.fn()}}))
const mockedAPI = vi.mocked(api)
const user: Principal = {user_id: 'u1', login_id: 'hong', display_name: '홍길동', role: 'user', auth_method: 'local'}
function mount(node = <MCPKeysPage/>, principal = user) {
  const query = new QueryClient({defaultOptions: {queries: {retry: false}, mutations: {retry: false}}})
  return render(<QueryClientProvider client={query}><SessionContext.Provider value={principal}>{node}</SessionContext.Provider></QueryClientProvider>)
}
beforeEach(() => {vi.clearAllMocks(); vi.spyOn(window, 'confirm').mockReturnValue(false)})
afterEach(() => {cleanup(); vi.restoreAllMocks()})

describe('MCP least-privilege credentials', () => {
  it('accepts null key lists and edits null scopes as deny-all without granting defaults', async () => {
    mockedAPI.mockResolvedValueOnce({keys:null})
    const view = mount()
    expect(await screen.findByText('등록된 MCP 키가 없습니다')).toBeInTheDocument()
    view.unmount()
    mockedAPI.mockResolvedValue({keys:[{id:'empty-key',user_id:'u1',name:'권한 없는 키',key_prefix:'mk_empty',status:'active',scopes:null}]})
    mount()
    await userEvent.setup().click(await screen.findByRole('button',{name:'권한 편집'}))
    const editor = screen.getByRole('region',{name:'MCP 키 권한 편집'})
    expect(within(editor).getAllByRole('checkbox').every(input => !(input as HTMLInputElement).checked)).toBe(true)
    expect(mockedAPI.mock.calls.some(([,options])=>options?.method)).toBe(false)
  })
  it.each([{keys:{}},{keys:[null]},{keys:[{id:'bad',scopes:{}}]}])('shows a safe failure for malformed key metadata: %j', async response => {
    mockedAPI.mockResolvedValue(response); mount()
    expect(await screen.findByRole('alert')).toHaveTextContent('서버 응답 형식')
  })
  it('does not mutate on render and creates only explicitly selected scopes; shows raw key once', async () => {
    mockedAPI.mockImplementation(async (_path, options) => options?.method === 'POST' ? {key: {id: 'new'}, raw_key: 'mk_test-secret-visible-once'} : {keys: []})
    mount()
    await screen.findByText('등록된 MCP 키가 없습니다')
    expect(mockedAPI.mock.calls.some(([, options]) => options?.method)).toBe(false)
    const person = userEvent.setup()
    await person.type(screen.getByLabelText('클라이언트·키 이름'), '회사 도구')
    await person.click(screen.getByRole('button', {name: '키 발급'}))
    expect(mockedAPI).toHaveBeenCalledWith('/api/mcp-keys', {method: 'POST', body: {name: '회사 도구', scopes: ['mail.read', 'mail.search']}})
    expect(await screen.findByDisplayValue('mk_test-secret-visible-once')).toBeInTheDocument()
    await person.click(screen.getByRole('button', {name: '키 표시 닫기'}))
    expect(screen.queryByDisplayValue('mk_test-secret-visible-once')).not.toBeInTheDocument()
    expect(localStorage.getItem('mk_test-secret-visible-once')).toBeNull()
  })

  it('preserves legacy scopes for review and requires confirmation to save explicit deny-all', async () => {
    mockedAPI.mockResolvedValue({keys: [{id: 'k1', user_id: 'u1', name: '기존 도구', key_prefix: 'mk_pre...', status: 'active', scopes: ['mail.read'], legacy_scopes: true}]})
    mount()
    const person = userEvent.setup()
    await screen.findByText('기존 키 호환 권한')
    await person.click(screen.getByRole('button', {name: '권한 편집'}))
    await person.click(within(screen.getByRole('region', {name: 'MCP 키 권한 편집'})).getByRole('checkbox', {name: /메일 읽기/}))
    await person.click(screen.getByRole('button', {name: '권한 저장'}))
    expect(mockedAPI.mock.calls.some(([, options]) => options?.method === 'PATCH')).toBe(false)
    vi.mocked(window.confirm).mockReturnValue(true)
    await person.click(screen.getByRole('button', {name: '권한 저장'}))
    await waitFor(() => expect(mockedAPI).toHaveBeenCalledWith('/api/mcp-keys/k1', {method: 'PATCH', body: {scopes: []}}))
  })

  it('does not fetch administrator inventories for ordinary users', () => {
    mount(<MCPAdminPage/>)
    expect(screen.getByText('관리자 권한이 필요합니다')).toBeInTheDocument()
    expect(mockedAPI).not.toHaveBeenCalled()
  })
})
