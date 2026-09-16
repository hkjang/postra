import {afterEach, it, expect, vi} from 'vitest'
import {api, csrfToken} from './client'
import {InvalidResponseError} from './response'
afterEach(() => {vi.unstubAllGlobals(); document.cookie = 'postra_csrf=; max-age=0; path=/'})
it.each(['', '<html>private upstream error</html>', '{"secret": "private-test-value"'])('rejects malformed successful JSON without exposing the body: %s', async body => {
  vi.stubGlobal('fetch', vi.fn().mockResolvedValue(new Response(body, {status: 200})))
  await expect(api('/api/action-cards')).rejects.toEqual(new InvalidResponseError())
})
it('preserves genuine no-content mutations and explicit nullable JSON', async () => {
  vi.stubGlobal('fetch', vi.fn().mockResolvedValueOnce(new Response(null, {status: 204})).mockResolvedValueOnce(new Response('null', {status: 200})))
  await expect(api('/auth/logout', {method: 'POST'})).resolves.toBeUndefined()
  await expect(api('/api/example')).resolves.toBeNull()
})
it('sends cookie credentials and a CSRF header, never a bearer token', async () => {
  document.cookie = 'postra_csrf=csrf-test; path=/'
  const fetch = vi.fn().mockResolvedValue(new Response('{"ok":true}', {status: 200}))
  vi.stubGlobal('fetch', fetch)
  expect(csrfToken()).toBe('csrf-test')
  await api('/api/drafts', {body: {subject: '안내'}})
  const [, options] = fetch.mock.calls[0]
  expect(options.credentials).toBe('same-origin')
  expect(options.headers['X-CSRF-Token']).toBe('csrf-test')
  expect(options.headers.Authorization).toBeUndefined()
  expect(options.body).toBe('{"subject":"안내"}')
})
it('never places a secret request or unexpected server HTML into an exception', async () => {
  vi.stubGlobal('fetch', vi.fn().mockResolvedValue(new Response('<html>unexpected upstream body</html>', {status: 502})))
  await expect(api('/api/admin/ai', {method: 'PUT', body: {api_key: 'private-test-value'}})).rejects.toThrow('요청을 처리하지 못했습니다 (502).')
})
it('rejects non-API URLs before making a request', async () => {
  const fetch = vi.fn(); vi.stubGlobal('fetch', fetch)
  await expect(api('https://third-party.invalid/upload')).rejects.toThrow('API 경로')
  expect(fetch).not.toHaveBeenCalled()
})
it('supports same-origin JSON auth without broadcasting expected login failures', async () => {
  const expired = vi.fn()
  window.addEventListener('postra:unauthorized', expired)
  const fetch = vi.fn().mockResolvedValue(new Response('{"code":"unauthorized","message":"로그인 정보를 확인하세요."}', {status: 401}))
  vi.stubGlobal('fetch', fetch)
  try {
    await expect(api('/auth/login', {body: {login_id: 'test', password: 'private-test-password'}})).rejects.toThrow('로그인 정보를 확인하세요.')
    expect(expired).not.toHaveBeenCalled()
    expect(fetch.mock.calls[0][1].credentials).toBe('same-origin')
    expect(fetch.mock.calls[0][1].headers.Authorization).toBeUndefined()
  } finally { window.removeEventListener('postra:unauthorized', expired) }
})
it('broadcasts resource authentication failures without recursively broadcasting session refresh failures', async () => {
  const expired = vi.fn()
  window.addEventListener('postra:unauthorized', expired)
  vi.stubGlobal('fetch', vi.fn().mockImplementation(async () => new Response('{"message":"로그인이 필요합니다"}', {status: 401})))
  try {
    await expect(api('/api/messages')).rejects.toThrow('로그인이 필요합니다')
    expect(expired).toHaveBeenCalledTimes(1)
    await expect(api('/auth/session')).rejects.toThrow('로그인이 필요합니다')
    expect(expired).toHaveBeenCalledTimes(1)
  } finally {window.removeEventListener('postra:unauthorized', expired)}
})
