import {act, cleanup, render, waitFor} from '@testing-library/react'
import {QueryClient, QueryClientProvider} from '@tanstack/react-query'
import {afterEach, beforeEach, describe, expect, it, vi} from 'vitest'
import {toast} from 'sonner'
import {api} from '@/api/client'
import {SessionContext, type Principal} from '@/app/session'
import {NotificationEvents, notificationChanges, type NotificationSnapshot} from './index'

vi.mock('@/api/client', () => ({api: vi.fn()}))
vi.mock('sonner', () => ({toast: {info: vi.fn(), error: vi.fn(), dismiss: vi.fn()}}))
class Stream {
  static instances: Stream[] = []
  closed = false
  onopen: (() => void) | null = null
  onerror: (() => void) | null = null
  handlers = new Map<string, (event: MessageEvent) => void>()
  constructor(public url: string) {Stream.instances.push(this)}
  addEventListener(name: string, callback: (event: MessageEvent) => void) {this.handlers.set(name, callback)}
  close() {this.closed = true}
  emit(name: string, value?: unknown) {this.handlers.get(name)?.(new MessageEvent(name, {data: JSON.stringify(value)}))}
}
const user: Principal = {user_id: 'notify-user', login_id: 'hong', display_name: '홍길동', role: 'user', auth_method: 'local'}
const base: NotificationSnapshot = {user_id: user.user_id, enabled: true, poll_seconds: 15, events: [{id: 'job:j1', category: 'sync', kind: 'job', resource_id: 'j1', status: 'running', at: 1}]}
function mount() {
  const cache = new QueryClient({defaultOptions: {queries: {retry: false}}})
  const view = render(<QueryClientProvider client={cache}><SessionContext.Provider value={user}><NotificationEvents/></SessionContext.Provider></QueryClientProvider>)
  return {...view, cache}
}
beforeEach(() => {vi.clearAllMocks(); Stream.instances = []; vi.stubGlobal('EventSource', Stream); vi.mocked(api).mockResolvedValue(base)})
afterEach(() => {cleanup(); vi.unstubAllGlobals(); vi.restoreAllMocks()})

describe('private metadata notifications', () => {
  it('does not replay initial state or disabled categories, and isolates identity changes', () => {
    expect(notificationChanges(undefined, base)).toEqual([])
    expect(notificationChanges(base, {...base, user_id: 'different-user'})).toEqual([])
    expect(notificationChanges(base, {...base, enabled: false, events: []})).toEqual([])
    expect(notificationChanges({...base, enabled: false, events: []}, base)).toEqual([])
    expect(notificationChanges(base, {...base, events: [{...base.events[0], at: 2}]})).toEqual([])
    expect(notificationChanges(base, {...base, events: [{...base.events[0], status: 'succeeded'}]})).toHaveLength(1)
  })

  it('opens a cookie-only same-origin stream, shows actual status changes once, and ignores other-user events', async () => {
    const view = mount()
    await waitFor(() => expect(view.cache.getQueryData(['notifications', user.user_id])).toEqual(base))
    expect(Stream.instances).toHaveLength(1)
    const stream = Stream.instances[0]
    expect(stream.url).toBe('/api/events')
    expect(toast.info).not.toHaveBeenCalled()
    const completed = {...base, events: [{...base.events[0], status: 'succeeded'}]}
    act(() => {stream.onopen?.(); stream.emit('snapshot', completed)})
    await waitFor(() => expect(toast.info).toHaveBeenCalledWith('메일 동기화 · 완료', {id: 'notifications:notify-user:job:j1'}))
    act(() => {stream.emit('snapshot', completed); stream.emit('snapshot', {...base, user_id: 'other-user', events: [{...base.events[0], id: 'private-job', status: 'failed'}]})})
    expect(toast.info).toHaveBeenCalledTimes(1)
    expect(toast.error).not.toHaveBeenCalled()
    expect(vi.mocked(api).mock.calls.every(([, options]) => !options?.method && options?.body === undefined)).toBe(true)
    view.unmount()
    expect(stream.closed).toBe(true)
    expect(toast.dismiss).toHaveBeenCalledWith('notifications:notify-user:job:j1')
  })

  it('closes an expired stream and triggers authoritative session recovery without echoing server data', async () => {
    const expired = vi.fn()
    window.addEventListener('postra:unauthorized', expired)
    const view = mount()
    await waitFor(() => expect(view.cache.getQueryData(['notifications', user.user_id])).toEqual(base))
    const stream = Stream.instances[0]
    act(() => stream.emit('session_expired', {message: 'do-not-display-provider-key'}))
    expect(expired).toHaveBeenCalledTimes(1)
    expect(stream.closed).toBe(true)
    expect(toast.error).not.toHaveBeenCalled()
    window.removeEventListener('postra:unauthorized', expired)
  })

  it('refreshes effective preferences when a revision changes even with activity notifications disabled', async () => {
    const disabled = {...base, enabled: false, events: [], preferences_revision: 'initial'}
    vi.mocked(api).mockResolvedValue(disabled)
    const view = mount()
    await waitFor(() => expect(view.cache.getQueryData(['notifications', user.user_id])).toEqual(disabled))
    const invalidate = vi.spyOn(view.cache, 'invalidateQueries')
    act(() => Stream.instances[0].emit('snapshot', {...disabled, preferences_revision: 'policy-changed'}))
    await waitFor(() => expect(invalidate).toHaveBeenCalledWith({queryKey: ['preferences']}))
    expect(toast.info).not.toHaveBeenCalled()
  })
})
