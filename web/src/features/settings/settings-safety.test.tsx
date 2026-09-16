import {act, cleanup, fireEvent, render, screen, waitFor, within} from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import {QueryClient, QueryClientProvider} from '@tanstack/react-query'
import {createMemoryRouter, Link, RouterProvider} from 'react-router-dom'
import {afterEach, beforeEach, describe, expect, it, vi} from 'vitest'
import {api} from '@/api/client'
import {OperationsConsole} from '@/features/admin/OperationsConsole'
import {SettingsEditor} from './SettingsEditor'
import type {SettingField, SettingsView} from './preferences'

vi.mock('@/api/client', () => ({api: vi.fn()}))
vi.mock('sonner', () => ({toast: {success: vi.fn(), error: vi.fn()}}))
vi.mock('@/features/admin/users', () => ({UsersPanel: () => <h2>사용자 목록</h2>, ProvisioningPanel: () => null, PurgePanel: () => null}))
vi.mock('@/features/admin/activity', () => ({AuditPanel: () => <h2>감사 기록 목록</h2>, IncidentsPanel: () => null}))
vi.mock('@/features/mcp', () => ({MCPAdminPage: () => null}))

const mockAPI = vi.mocked(api)
const field = (partial: Partial<SettingField>): SettingField => ({key: 'ai.base_url', label: 'AI Base URL', category: 'ai', type: 'url', value: 'https://ai.test/v1', default: '', scope: 'admin', apply: 'live', secret: false, source: 'admin', locked: false, lockable: false, ...partial})
let view: SettingsView
beforeEach(() => {
  view = {revision: 'r1', fields: [field({}), field({key: 'ai.api_key_ref', label: 'AI API Key', type: 'secret', secret: true, registered: true, value: ''})]}
  mockAPI.mockReset()
  mockAPI.mockImplementation(async (path, options) => {
    if (path === '/api/accounts' || path === '/api/signatures') return [] as never
    if (path === '/api/admin/operations') return {ai: 'configured'} as never
    if (options?.method === 'PATCH') return {...view, revision: 'r2'} as never
    return view as never
  })
})
afterEach(() => {cleanup(); vi.restoreAllMocks()})

function mount(console = false, category = 'ai') {
  const cache = new QueryClient({defaultOptions: {queries: {retry: false}}})
  const router = createMemoryRouter([
    {path: '/', element: <><Link to="/away">다른 화면</Link>{console ? <OperationsConsole/> : <SettingsEditor admin/>}</>},
    {path: '/away', element: <h1>이동 완료</h1>},
  ], {initialEntries: [`/?category=${category}`]})
  render(<QueryClientProvider client={cache}><RouterProvider router={router}/></QueryClientProvider>)
  return {cache, router}
}

describe('settings draft retention', () => {
  it.each(['users', 'audit'])('retains pending credentials when search is cleared in %s', async category => {
    const user = userEvent.setup(); mount(true, category)
    await user.type(screen.getByLabelText('관리자 설정 검색'), 'AI API Key')
    await user.type(await screen.findByLabelText('AI API Key'), 'pending-secret')
    await user.clear(screen.getByLabelText('관리자 설정 검색'))
    expect(screen.getByRole('button', {name: '변경 확인'})).toBeEnabled()
    await user.type(screen.getByLabelText('관리자 설정 검색'), 'AI API Key')
    expect(screen.getByLabelText('AI API Key')).toHaveValue('pending-secret')
    await user.click(screen.getByRole('link', {name: '다른 화면'}))
    expect(await screen.findByRole('dialog')).not.toHaveTextContent('pending-secret')
    await user.click(screen.getByRole('button', {name: '계속 편집'}))
    expect(screen.getByLabelText('AI API Key')).toHaveValue('pending-secret')
    expect(mockAPI.mock.calls.some(([, options]) => options?.method === 'PATCH')).toBe(false)
  })

  it('keeps the search and pending value after a canceled category change', async () => {
    const user = userEvent.setup(); mount(true, 'users')
    await user.type(screen.getByLabelText('관리자 설정 검색'), 'AI API Key')
    await user.type(await screen.findByLabelText('AI API Key'), 'pending-secret')
    await user.click(screen.getByRole('button', {name: '발송 정책'}))
    await user.click(await screen.findByRole('button', {name: '계속 편집'}))
    expect(screen.getByLabelText('관리자 설정 검색')).toHaveValue('AI API Key')
    expect(screen.getByLabelText('AI API Key')).toHaveValue('pending-secret')
    await user.click(screen.getByRole('button', {name: '발송 정책'}))
    await user.click(await screen.findByRole('button', {name: '변경 버리고 이동'}))
    expect(screen.getByLabelText('관리자 설정 검색')).toHaveValue('')
    expect(screen.getByRole('button', {name: '변경 확인'})).toBeDisabled()
  })

  it('keeps the form and navigation guard when a cached settings refresh fails', async () => {
    const user = userEvent.setup(); const {cache} = mount()
    await user.type(await screen.findByLabelText('AI API Key'), 'preserved-secret')
    mockAPI.mockRejectedValueOnce(new Error('설정을 새로 가져오지 못했습니다.'))
    await act(async () => {await cache.refetchQueries({queryKey: ['configuration']})})
    expect(await screen.findByText('설정을 새로 가져오지 못했습니다.')).toBeInTheDocument()
    expect(screen.getByLabelText('AI API Key')).toHaveValue('preserved-secret')
    await user.click(screen.getByRole('link', {name: '다른 화면'}))
    expect(await screen.findByRole('dialog')).not.toHaveTextContent('preserved-secret')
    await user.click(screen.getByRole('button', {name: '계속 편집'}))
    expect(screen.getByLabelText('AI API Key')).toHaveValue('preserved-secret')
  })
})

describe('settings save and logout safety', () => {
  it('also locks a clean form while logout is pending and restores editing if logout fails', async () => {
    mount()
    const input = await screen.findByLabelText('AI Base URL')
    const confirm = vi.spyOn(window, 'confirm')
    expect(fireEvent(window, new Event('postra:before-logout', {cancelable: true}))).toBe(true)
    expect(confirm).not.toHaveBeenCalled()
    expect(input).toBeDisabled()
    expect(screen.getByRole('button', {name: '변경 확인'})).toBeDisabled()
    fireEvent.change(input, {target: {value: 'https://must-not-save.test/v1'}})
    expect(input).toHaveValue('https://ai.test/v1')
    expect(mockAPI.mock.calls.some(([, options]) => options?.method === 'PATCH')).toBe(false)
    fireEvent(window, new Event('postra:logout-failed'))
    expect(input).toBeEnabled()
    fireEvent.change(input, {target: {value: 'https://can-edit-again.test/v1'}})
    expect(input).toHaveValue('https://can-edit-again.test/v1')
    expect(screen.getByRole('button', {name: '변경 확인'})).toBeEnabled()
  })

  it('cancels logout before revocation, restores protection after failure, and avoids duplicate unload confirmation', async () => {
    const user = userEvent.setup(); mount()
    await user.type(await screen.findByLabelText('AI API Key'), 'unsaved-private-key')
    const confirm = vi.spyOn(window, 'confirm').mockReturnValue(false)
    expect(fireEvent(window, new Event('postra:before-logout', {cancelable: true}))).toBe(false)
    expect(confirm).toHaveBeenCalledOnce()
    expect(confirm.mock.calls[0][0]).not.toContain('unsaved-private-key')
    expect(screen.getByLabelText('AI API Key')).toHaveValue('unsaved-private-key')
    confirm.mockReturnValue(true)
    expect(fireEvent(window, new Event('postra:before-logout', {cancelable: true}))).toBe(true)
    expect(screen.getByLabelText('AI API Key')).toBeDisabled()
    expect(screen.getByRole('button', {name: '변경 확인'})).toBeDisabled()
    expect(screen.getByRole('button', {name: '변경 취소'})).toBeDisabled()
    expect(fireEvent(window, new Event('beforeunload', {cancelable: true}))).toBe(true)
    fireEvent(window, new Event('postra:logout-failed'))
    expect(fireEvent(window, new Event('beforeunload', {cancelable: true}))).toBe(false)
    expect(screen.getByLabelText('AI API Key')).toHaveValue('unsaved-private-key')
    expect(screen.getByLabelText('AI API Key')).toBeEnabled()
    expect(screen.getByRole('button', {name: '변경 확인'})).toBeEnabled()
    confirm.mockReturnValue(false)
    expect(fireEvent(window, new Event('postra:before-logout', {cancelable: true}))).toBe(false)
    expect(mockAPI.mock.calls.some(([path]) => path === '/auth/logout')).toBe(false)
  })

  it('blocks logout and discarding navigation while a save is pending', async () => {
    const user = userEvent.setup(); const {router} = mount()
    await user.type(await screen.findByLabelText('AI API Key'), 'pending-save-key')
    let resolve!: (value: SettingsView) => void
    const pending = new Promise<SettingsView>(done => {resolve = done})
    mockAPI.mockImplementationOnce(async () => await pending as never)
    await user.click(screen.getByRole('button', {name: '변경 확인'}))
    await user.click(screen.getByRole('button', {name: '확인 후 저장'}))
    expect(fireEvent(window, new Event('postra:before-logout', {cancelable: true}))).toBe(false)
    expect(fireEvent(window, new Event('beforeunload', {cancelable: true}))).toBe(false)
    await act(async () => {await router.navigate('/away')})
    expect(await screen.findByText('설정을 저장하고 있습니다')).toBeInTheDocument()
    expect(screen.getByRole('button', {name: '변경 버리고 이동'})).toBeDisabled()
    await act(async () => {resolve({...view, revision: 'r2'}); await pending})
    await waitFor(() => expect(screen.queryByText('설정을 저장하고 있습니다')).not.toBeInTheDocument())
    expect(screen.queryByRole('heading', {name: '이동 완료'})).not.toBeInTheDocument()
  })

  it('does not report a cleared secret as saved through an empty retry', async () => {
    const user = userEvent.setup(); mount()
    const input = await screen.findByLabelText('AI API Key')
    await user.type(input, 'failed-secret')
    await user.click(screen.getByRole('button', {name: '변경 확인'}))
    mockAPI.mockRejectedValueOnce(new Error('안전한 저장 실패'))
    await user.click(screen.getByRole('button', {name: '확인 후 저장'}))
    await waitFor(() => expect(input).toHaveValue(''))
    const dialog = screen.getByRole('dialog')
    expect(dialog).toHaveTextContent('입력한 비밀값은 지워졌습니다')
    expect(dialog).not.toHaveTextContent('failed-secret')
    expect(screen.queryByText('모든 변경사항이 저장되었습니다.')).not.toBeInTheDocument()
    expect(screen.getByText('비밀값 저장이 완료되지 않았습니다. 다시 입력해 주세요.')).toBeInTheDocument()
    const retry = within(dialog).getByRole('button', {name: '확인 후 저장'})
    expect(retry).toBeDisabled()
    await user.click(retry)
    expect(mockAPI.mock.calls.filter(([, options]) => options?.method === 'PATCH')).toHaveLength(1)
  })

  it('allows retry of non-secret changes after clearing a failed secret', async () => {
    const user = userEvent.setup(); mount()
    const input = await screen.findByLabelText('AI Base URL')
    await user.clear(input); await user.type(input, 'https://new.test/v1')
    await user.type(screen.getByLabelText('AI API Key'), 'failed-secret')
    await user.click(screen.getByRole('button', {name: '변경 확인'}))
    mockAPI.mockRejectedValueOnce(new Error('안전한 저장 실패'))
    await user.click(screen.getByRole('button', {name: '확인 후 저장'}))
    await waitFor(() => expect(screen.getByRole('button', {name: '확인 후 저장'})).toBeEnabled())
    await user.click(screen.getByRole('button', {name: '확인 후 저장'}))
    const patches = mockAPI.mock.calls.filter(([, options]) => options?.method === 'PATCH')
    expect(patches).toHaveLength(2)
    expect(patches[1][1]?.body).toMatchObject({values: {'ai.base_url': 'https://new.test/v1'}, secrets: {}})
  })
})

describe('task model JSON safety', () => {
  it.each(['{"summarize":', 'null', '[]', '"private-invalid-value"', '{"summarize":null}', '{"summarize":[]}', '{"summarize":{"model":12}}', '{"summarize":{"max_tokens":"2048"}}'])('preserves malformed or incompatible JSON %s until corrected', async value => {
    view.fields = [field({key: 'ai.task_models', label: '작업별 모델', type: 'json', value})]
    const user = userEvent.setup(); mount()
    const advanced = await screen.findByLabelText('작업별 모델 JSON')
    expect(advanced).toHaveValue(value)
    expect(advanced).toHaveAttribute('aria-invalid', 'true')
    expect(screen.getByLabelText('메일 요약')).toBeDisabled()
    expect(screen.getByRole('alert')).not.toHaveTextContent(value)
    fireEvent.change(advanced, {target: {value: '{"summarize":{"model":"summary-model","base_url":"https://route.test/v1"}}'}})
    expect(screen.queryByRole('alert')).not.toBeInTheDocument()
    const simple = screen.getByLabelText('메일 요약')
    expect(simple).toBeEnabled()
    await user.clear(simple); await user.type(simple, 'new-model')
    expect(JSON.parse((advanced as HTMLTextAreaElement).value)).toEqual({summarize: {model: 'new-model', base_url: 'https://route.test/v1'}})
    expect(mockAPI.mock.calls.some(([, options]) => options?.method === 'PATCH')).toBe(false)
  })
})
