import {afterEach, beforeEach, describe, expect, it, vi} from 'vitest'
import {act, cleanup, render, screen, waitFor} from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import {QueryClient, QueryClientProvider} from '@tanstack/react-query'
import {MemoryRouter} from 'react-router-dom'
import type {ReactNode} from 'react'
import {api} from '@/api/client'
import {OperationsConsole} from '@/features/admin/OperationsConsole'
import {SettingsEditor} from './SettingsEditor'
import type {SettingField, SettingsView} from './preferences'

vi.mock('@/api/client', () => ({api: vi.fn()}))
vi.mock('sonner', () => ({toast: {success: vi.fn(), error: vi.fn()}}))
const mockAPI = vi.mocked(api)
const field = (value: Partial<SettingField>): SettingField => ({key: 'ai.model', label: 'Chat Model', category: 'ai', type: 'string', value: 'saved-chat', default: '', scope: 'admin', apply: 'live', secret: false, source: 'admin', locked: false, lockable: false, ...value})
const detected = {model: 'candidate-chat', context_length: 131072, max_output_tokens: 8192, source: 'models', status: 'detected'}
const diagnostic = (limits: unknown = detected) => ({ok: true, message: '모델 정보 확인 완료', model: 'candidate-chat', latency_ms: 12, limits})
let view: SettingsView
let response: unknown
beforeEach(() => {
  view = {revision: 'r1', fields: [
    field({}),
    field({key: 'ai.base_url', label: 'AI Base URL', type: 'url', value: 'https://saved.test/v1'}),
    field({key: 'ai.api_key_ref', label: 'AI API Key', type: 'secret', secret: true, registered: true, value: ''}),
    field({key: 'ai.auto_context_length', label: '컨텍스트 자동 감지', type: 'bool', value: 'true'}),
    field({key: 'ai.context_length', label: '컨텍스트 설정값', type: 'int', value: '32768'}),
    field({key: 'ai.task_models', label: '작업별 모델', type: 'json', value: '{}'}),
    field({key: 'ai.embed_model', label: 'Embedding Model', category: 'search', value: 'saved-embed'}),
  ]}
  response = diagnostic()
  mockAPI.mockReset()
  mockAPI.mockImplementation(async (path, options) => {
    if (path === '/api/accounts' || path === '/api/signatures') return [] as never
    if (path === '/api/admin/operations') return {ai: 'configured'} as never
    if (options?.method === 'PATCH') return {...view, revision: 'r2'} as never
    if (options?.method === 'POST') return await response as never
    return view as never
  })
})
afterEach(cleanup)

function show(category = 'ai', console = false) {
  const cache = new QueryClient({defaultOptions: {queries: {retry: false}}})
  const wrapper = ({children}: {children: ReactNode}) => <QueryClientProvider client={cache}><MemoryRouter initialEntries={[`/?category=${category}`]}>{children}</MemoryRouter></QueryClientProvider>
  return render(console ? <OperationsConsole/> : <SettingsEditor admin category={category}/>, {wrapper})
}
function deferred() {
  let resolve!: (value: unknown) => void
  let reject!: (reason: unknown) => void
  const promise = new Promise((resolvePromise, rejectPromise) => {resolve = resolvePromise; reject = rejectPromise})
  return {promise, resolve, reject}
}

describe('candidate model limit discovery', () => {
  it('queries only metadata with unsaved endpoint, model, credentials and task routing', async () => {
    const user = userEvent.setup(); show()
    const model = await screen.findByLabelText('Chat Model')
    expect(mockAPI.mock.calls.some(([, options]) => options?.method === 'POST')).toBe(false)
    await user.clear(model); await user.type(model, 'candidate-chat')
    const endpoint = screen.getByLabelText('AI Base URL')
    await user.clear(endpoint); await user.type(endpoint, 'https://candidate.test/v1')
    await user.type(screen.getByLabelText('AI API Key'), 'candidate-secret')
    await user.type(screen.getByLabelText('메일 요약'), 'summary-chat')
    await user.selectOptions(screen.getByLabelText('Chat 진단 작업'), 'summarize')
    await user.click(screen.getByRole('button', {name: '저장 전 Chat 모델 한도 조회'}))
    expect(await screen.findByText('자동 감지')).toBeInTheDocument()
    expect(screen.getByRole('status')).toHaveTextContent('131,072 토큰')
    expect(screen.getByRole('status')).toHaveTextContent('8,192 토큰')
    expect(screen.getByRole('status')).toHaveTextContent('서버 /models')
    expect(screen.getByRole('status')).toHaveTextContent('최대 출력은 설정·작업별 한도와 서버 한도 중 작은 값을 적용')
    expect(mockAPI).toHaveBeenCalledWith('/api/admin/configuration/test', expect.objectContaining({method: 'POST', body: {
      target: 'ai_models', task: 'summarize',
      values: {'ai.model': 'candidate-chat', 'ai.base_url': 'https://candidate.test/v1', 'ai.task_models': '{"summarize":{"model":"summary-chat"}}'},
      secrets: {'ai.api_key_ref': 'candidate-secret'},
    }}))
    expect(screen.getByLabelText('AI API Key')).toHaveValue('candidate-secret')
    expect(mockAPI.mock.calls.filter(([, options]) => options?.method === 'POST')).toHaveLength(1)
    expect(mockAPI.mock.calls.some(([, options]) => options?.method === 'PATCH')).toBe(false)
  })

  it('queries embedding metadata from search settings without applying the Chat task', async () => {
    const user = userEvent.setup(); show('search')
    const model = await screen.findByLabelText('Embedding Model')
    await user.clear(model); await user.type(model, 'candidate-embed')
    await user.selectOptions(screen.getByLabelText('Chat 진단 작업'), 'qa')
    await user.click(screen.getByRole('button', {name: '저장 전 Embedding 모델 한도 조회'}))
    await screen.findByText('자동 감지')
    expect(mockAPI).toHaveBeenCalledWith('/api/admin/configuration/test', expect.objectContaining({body: {target: 'embedding_models', values: {'ai.embed_model': 'candidate-embed'}, secrets: {}}}))
  })

  it.each(['unavailable', 'model_not_found'])('labels %s as configuration fallback, not a connection failure', async status => {
    response = {...diagnostic({...detected, status, source: 'config', context_length: 32768, max_output_tokens: 0}), ok: false}
    const user = userEvent.setup(); show()
    await user.click(await screen.findByRole('button', {name: '저장 전 Chat 모델 한도 조회'}))
    const result = await screen.findByRole('status')
    expect(result).toHaveTextContent('설정값 대체')
    expect(result).toHaveTextContent('32,768 토큰')
    expect(result).toHaveTextContent('최대 출력: 미제공')
    expect(result).toHaveTextContent('출처: 설정값')
    expect(result).not.toHaveTextContent('연결 확인 필요')
    expect(result).not.toHaveTextContent('연결 성공')
    expect(screen.queryByRole('alert')).not.toBeInTheDocument()
  })

  it('shows manual limits when auto detection is disabled without persisting the toggle', async () => {
    response = diagnostic({...detected, status: 'manual', source: 'config', context_length: 32768})
    const user = userEvent.setup(); show()
    await user.click(await screen.findByLabelText('컨텍스트 자동 감지'))
    await user.click(screen.getByRole('button', {name: '저장 전 Chat 모델 한도 조회'}))
    expect(await screen.findByText('수동 설정')).toBeInTheDocument()
    expect(screen.getByRole('status')).toHaveTextContent('자동 감지가 꺼져 있어')
    expect(mockAPI).toHaveBeenCalledWith('/api/admin/configuration/test', expect.objectContaining({body: {target: 'ai_models', values: {'ai.auto_context_length': 'false'}, secrets: {}}}))
  })

  it.each(['unavailable', 'manual'])('does not imply a configured embedding Context cap when %s metadata has no limit', async status => {
    response = diagnostic({...detected, model: 'embed', context_length: 0, max_output_tokens: 0, source: 'config', status})
    const user = userEvent.setup(); show('search')
    await user.click(await screen.findByRole('button', {name: '저장 전 Embedding 모델 한도 조회'}))
    const result = await screen.findByRole('status')
    expect(result).toHaveTextContent('컨텍스트 미제공')
    expect(result).toHaveTextContent('컨텍스트: 미제공')
    expect(result).toHaveTextContent('컨텍스트 출처: 미제공')
    expect(result).not.toHaveTextContent('설정값 대체')
    expect(result).not.toHaveTextContent('설정한 한도를 사용')
  })

  it('keeps the full connection test and reports its success independently of metadata fallback', async () => {
    response = diagnostic({...detected, status: 'unavailable', source: 'config'})
    const user = userEvent.setup(); show()
    await user.selectOptions(await screen.findByLabelText('Chat 진단 작업'), 'compose')
    await user.click(screen.getByRole('button', {name: '저장 전 AI 연결 확인'}))
    expect(await screen.findByText('연결 성공')).toBeInTheDocument()
    expect(screen.getByRole('status')).toHaveTextContent('설정값 대체')
    expect(mockAPI).toHaveBeenCalledWith('/api/admin/configuration/test', expect.objectContaining({body: {target: 'ai', task: 'compose', values: {}, secrets: {}}}))
  })

  it('accepts an older full connection response without optional model limits', async () => {
    response = {ok: true, message: 'legacy probe success', model: 'saved-chat', latency_ms: 3}
    const user = userEvent.setup(); show()
    await user.click(await screen.findByRole('button', {name: '저장 전 AI 연결 확인'}))
    expect(await screen.findByText('연결 성공')).toBeInTheDocument()
    expect(screen.getByRole('status')).toHaveTextContent('모델 한도 정보 미제공')
  })

  it.each([['auth', '저장 전 OIDC Discovery 확인'], ['storage', '현재 Database 연결 확인']])('does not show irrelevant Chat model metadata for %s diagnostics', async (category, button) => {
    const user = userEvent.setup(); show(category)
    await user.click(await screen.findByRole('button', {name: button}))
    expect(await screen.findByText('연결 성공')).toBeInTheDocument()
    expect(screen.getByRole('status')).not.toHaveTextContent('candidate-chat')
    expect(screen.queryByText('자동 감지')).not.toBeInTheDocument()
    expect(screen.queryByText(/모델 한도 정보 미제공/)).not.toBeInTheDocument()
  })

  it.each([null, [], {ok: true, message: {secret: 'private-response'}}, {ok: 'yes', message: 'private-response'}, diagnostic({...detected, context_length: '131072'}), diagnostic({...detected, status: 'private-response'})])('rejects malformed success payloads safely (%#)', async payload => {
    response = payload
    const user = userEvent.setup(); show()
    await user.click(await screen.findByRole('button', {name: '저장 전 Chat 모델 한도 조회'}))
    expect(await screen.findByRole('alert')).toHaveTextContent('서버 응답 형식을 확인할 수 없습니다')
    expect(screen.queryByRole('status')).not.toBeInTheDocument()
    expect(screen.queryByText('private-response')).not.toBeInTheDocument()
  })
})

describe('diagnostic freshness', () => {
  it('clears displayed results and errors when a candidate or selected task changes', async () => {
    const user = userEvent.setup(); show()
    await user.click(await screen.findByRole('button', {name: '저장 전 Chat 모델 한도 조회'}))
    await screen.findByText('자동 감지')
    await user.type(screen.getByLabelText('AI API Key'), 'new-key')
    expect(screen.queryByRole('status')).not.toBeInTheDocument()
    response = null
    await user.click(screen.getByRole('button', {name: '저장 전 Chat 모델 한도 조회'}))
    await screen.findByRole('alert')
    await user.selectOptions(screen.getByLabelText('Chat 진단 작업'), 'rewrite')
    expect(screen.queryByRole('alert')).not.toBeInTheDocument()
  })

  it('ignores an older response and its completion while the new candidate is still loading', async () => {
    const old = deferred(); const next = deferred(); response = old.promise
    const user = userEvent.setup(); show()
    await user.click(await screen.findByRole('button', {name: '저장 전 Chat 모델 한도 조회'}))
    const firstSignal = mockAPI.mock.calls.find(([, options]) => options?.method === 'POST')![1]?.signal
    await user.type(screen.getByLabelText('Chat Model'), '-new')
    expect(firstSignal?.aborted).toBe(true)
    response = next.promise
    await user.click(screen.getByRole('button', {name: '저장 전 Chat 모델 한도 조회'}))
    await act(async () => {old.resolve({...diagnostic(), message: 'outdated result'})})
    expect(screen.queryByText(/outdated result/)).not.toBeInTheDocument()
    expect(screen.getByRole('button', {name: '저장 전 Chat 모델 한도 조회'})).toBeDisabled()
    await act(async () => {next.resolve({...diagnostic(), message: 'latest result'})})
    expect(await screen.findByText(/latest result/)).toBeInTheDocument()
  })

  it('clears category diagnostics and ignores a rejected request from the previous category', async () => {
    const pending = deferred(); response = pending.promise
    const user = userEvent.setup(); const editor = show()
    await user.click(await screen.findByRole('button', {name: '저장 전 Chat 모델 한도 조회'}))
    editor.rerender(<SettingsEditor admin category="search"/>)
    await screen.findByLabelText('Embedding Model')
    expect(screen.queryByRole('status')).not.toBeInTheDocument()
    await act(async () => {pending.reject(new Error('outdated failure'))})
    expect(screen.queryByRole('alert')).not.toBeInTheDocument()
    expect(screen.getByRole('button', {name: '저장 전 Embedding 모델 한도 조회'})).toBeEnabled()
  })

  it('clears a completed candidate diagnostic when saving', async () => {
    const user = userEvent.setup(); show()
    await user.type(await screen.findByLabelText('Chat Model'), '-new')
    await user.click(screen.getByRole('button', {name: '저장 전 Chat 모델 한도 조회'}))
    await screen.findByText('자동 감지')
    await user.click(screen.getByRole('button', {name: '변경 확인'}))
    await user.click(screen.getByRole('button', {name: '확인 후 저장'}))
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument())
    expect(screen.queryByRole('status')).not.toBeInTheDocument()
  })

  it('ignores an in-flight candidate result after saving', async () => {
    const pending = deferred(); response = pending.promise
    const user = userEvent.setup(); show()
    await user.type(await screen.findByLabelText('Chat Model'), '-new')
    await user.click(screen.getByRole('button', {name: '저장 전 Chat 모델 한도 조회'}))
    await user.click(screen.getByRole('button', {name: '변경 확인'}))
    await user.click(screen.getByRole('button', {name: '확인 후 저장'}))
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument())
    await act(async () => {pending.resolve(diagnostic())})
    expect(screen.queryByRole('status')).not.toBeInTheDocument()
  })
})

describe('saved operations diagnostic', () => {
  it('shows limits from the current Chat test and clears them after settings are saved', async () => {
    const user = userEvent.setup(); show('ai', true)
    await screen.findByLabelText('Chat Model')
    await user.click(screen.getByRole('button', {name: '현재 AI 연결 테스트'}))
    expect(await screen.findByText('연결 성공')).toBeInTheDocument()
    expect(screen.getByRole('status')).toHaveTextContent('131,072 토큰')
    expect(mockAPI).toHaveBeenCalledWith('/api/admin/ai/test', expect.objectContaining({method: 'POST'}))
    await user.type(screen.getByLabelText('Chat Model'), '-new')
    await user.click(screen.getByRole('button', {name: '변경 확인'}))
    await user.click(screen.getByRole('button', {name: '확인 후 저장'}))
    await waitFor(() => expect(screen.queryByRole('status')).not.toBeInTheDocument())
  })

  it('keeps the existing embedding/vector result without a common latency or model', async () => {
    response = {ok: true, message: 'embedding and vector healthy', ai_embed_model: 'embed', ai_embed_latency_ms: 5, vector_store_latency_ms: 2}
    const user = userEvent.setup(); show('search', true)
    await screen.findByLabelText('Embedding Model')
    await user.click(screen.getByRole('button', {name: '현재 임베딩·벡터 테스트'}))
    expect(await screen.findByText('연결 성공')).toBeInTheDocument()
    expect(screen.getByRole('status')).toHaveTextContent('embedding and vector healthy')
  })

  it('ignores a saved Chat diagnostic that completes after the settings revision changed', async () => {
    const pending = deferred(); response = pending.promise
    const user = userEvent.setup(); show('ai', true)
    await screen.findByLabelText('Chat Model')
    await user.click(screen.getByRole('button', {name: '현재 AI 연결 테스트'}))
    await user.type(screen.getByLabelText('Chat Model'), '-new')
    await user.click(screen.getByRole('button', {name: '변경 확인'}))
    await user.click(screen.getByRole('button', {name: '확인 후 저장'}))
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument())
    await act(async () => {pending.resolve(diagnostic())})
    expect(screen.queryByRole('status')).not.toBeInTheDocument()
    expect(screen.getByRole('button', {name: '현재 AI 연결 테스트'})).toBeEnabled()
  })

  it('rejects malformed saved diagnostic responses without rendering their content', async () => {
    response = {ok: true, message: {secret: 'private-response'}}
    const user = userEvent.setup(); show('ai', true)
    await screen.findByLabelText('Chat Model')
    await user.click(screen.getByRole('button', {name: '현재 AI 연결 테스트'}))
    expect(await screen.findByRole('alert')).toHaveTextContent('서버 응답 형식을 확인할 수 없습니다')
    expect(screen.queryByRole('status')).not.toBeInTheDocument()
  })
})
