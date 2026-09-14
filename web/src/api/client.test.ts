import {afterEach, it, expect, vi} from 'vitest'
import {api, csrfToken} from './client'
afterEach(() => {vi.unstubAllGlobals(); document.cookie = 'postra_csrf=; max-age=0; path=/'})
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
