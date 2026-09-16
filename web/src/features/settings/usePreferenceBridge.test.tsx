import {afterEach, describe, expect, it, vi} from 'vitest'
import {act, cleanup, render, screen, waitFor} from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import {QueryClient, QueryClientProvider} from '@tanstack/react-query'
import {MemoryRouter} from 'react-router-dom'
import {api} from '@/api/client'
import {SettingsEditor} from './SettingsEditor'
import {usePreferenceBridge} from './usePreferenceBridge'
import type {SettingsView} from './preferences'

vi.mock('@/api/client', () => ({api: vi.fn()}))
const mockAPI = vi.mocked(api)
function settings(value = 'standard', locked = false): SettingsView {
  return {revision: value, fields: [{key: 'ui.text_size', label: '화면 글자 크기', category: 'appearance', type: 'enum', value, default: 'standard', options: ['standard', 'large'], scope: 'user', apply: 'live', secret: false, source: locked ? 'admin_policy' : 'user', locked, lockable: true}]}
}
function Bridge() {usePreferenceBridge(); return null}
afterEach(() => {cleanup(); mockAPI.mockReset(); delete document.documentElement.dataset.textSize})

describe('workspace text size preference', () => {
  it('applies saved changes immediately and restores the server value on remount', async () => {
    let saved = settings()
    mockAPI.mockImplementation(async (path, options) => {
      if (path !== '/api/preferences') return [] as never
      if (options?.method === 'PATCH') saved = settings('large')
      return saved as never
    })
    const user = userEvent.setup()
    const cache = new QueryClient({defaultOptions: {queries: {retry: false}}})
    const first = render(<QueryClientProvider client={cache}><MemoryRouter><Bridge/><SettingsEditor/></MemoryRouter></QueryClientProvider>)
    const control = await screen.findByLabelText('화면 글자 크기')
    expect(document.documentElement.dataset.textSize).toBe('standard')
    await user.selectOptions(control, 'large')
    expect(document.documentElement.dataset.textSize).toBe('standard')
    await user.click(screen.getByRole('button', {name: '변경 확인'}))
    expect(screen.getByRole('dialog')).toHaveTextContent('화면 글자 크기')
    await user.click(screen.getByRole('button', {name: '확인 후 저장'}))
    await waitFor(() => expect(document.documentElement.dataset.textSize).toBe('large'))
    expect(mockAPI).toHaveBeenCalledWith('/api/preferences', expect.objectContaining({method: 'PATCH', body: expect.objectContaining({values: {'ui.text_size': 'large'}})}))
    first.unmount(); cache.clear()
    render(<QueryClientProvider client={new QueryClient({defaultOptions: {queries: {retry: false}}})}><Bridge/></QueryClientProvider>)
    await waitFor(() => expect(document.documentElement.dataset.textSize).toBe('large'))
  })

  it('uses effective policy values and falls back safely for missing or invalid values', async () => {
    mockAPI.mockResolvedValue(settings('large') as never)
    const cache = new QueryClient({defaultOptions: {queries: {retry: false, staleTime: Infinity}}})
    render(<QueryClientProvider client={cache}><MemoryRouter><Bridge/><SettingsEditor/></MemoryRouter></QueryClientProvider>)
    await waitFor(() => expect(document.documentElement.dataset.textSize).toBe('large'))
    act(() => cache.setQueryData(['preferences', 'user'], settings('standard', true)))
    await waitFor(() => expect(document.documentElement.dataset.textSize).toBe('standard'))
    expect(screen.getByLabelText('화면 글자 크기')).toBeDisabled()
    act(() => cache.setQueryData(['preferences', 'user'], settings('large')))
    await waitFor(() => expect(document.documentElement.dataset.textSize).toBe('large'))
    act(() => cache.setQueryData(['preferences', 'user'], {revision: 'other-user', fields: []}))
    await waitFor(() => expect(document.documentElement.dataset.textSize).toBe('standard'))
    act(() => cache.setQueryData(['preferences', 'user'], settings('unrecognized')))
    await waitFor(() => expect(document.documentElement.dataset.textSize).toBe('standard'))
  })
})
