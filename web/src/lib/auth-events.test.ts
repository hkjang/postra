import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { authReturnTo, claimSilentSSO, markSignedOut, notifyAuthChange, receiveAuthChange, resetSSOFlags } from './auth-events'

beforeEach(() => { resetSSOFlags(); window.localStorage.removeItem('postra-auth-change') })
afterEach(() => { vi.restoreAllMocks(); resetSSOFlags() })

describe('best-effort cross-tab authentication notification', () => {
  it('writes only a non-sensitive timestamp', () => {
    const write = vi.spyOn(Storage.prototype, 'setItem')
    vi.spyOn(Date, 'now').mockReturnValue(1234567890)
    notifyAuthChange()
    expect(write).toHaveBeenCalledExactlyOnceWith('postra-auth-change', '1234567890')
  })

  it('does not throw when browser policy denies storage writes', () => {
    vi.spyOn(Storage.prototype, 'setItem').mockImplementation(() => { throw new DOMException('Storage is disabled', 'SecurityError') })
    expect(notifyAuthChange).not.toThrow()
  })

  it('does not throw when obtaining localStorage is denied', () => {
    vi.spyOn(window, 'localStorage', 'get').mockImplementation(() => { throw new DOMException('Storage is disabled', 'SecurityError') })
    expect(notifyAuthChange).not.toThrow()
  })

  it('broadcasts a deliberate logout without storing any identity', () => {
    const write = vi.spyOn(Storage.prototype, 'setItem')
    vi.spyOn(Date, 'now').mockReturnValue(1234567890)
    notifyAuthChange('signed_out')
    expect(write).toHaveBeenCalledExactlyOnceWith('postra-auth-change', '1234567890:signed_out')
  })
})

describe('silent SSO tab guards shared with legacy UI', () => {
  it('claims one prompt=none attempt and preserves the current SPA return path', () => {
    const path = '/app/mail?message=m1&q=%ED%9A%8C%EC%9D%98'
    const first = claimSilentSSO(path, '?message=m1&q=%ED%9A%8C%EC%9D%98')
    expect(first).toBe('/auth/oidc/start?prompt=none&return_to=' + encodeURIComponent(path))
    expect(window.sessionStorage.getItem('postra.sso.silentAttempted')).toBe('true')
    expect(claimSilentSSO(path, '')).toBeUndefined()
  })

  it.each(['?sso=none', '?sso=signed_out', '?sso=error', '?sso='])('does not retry on a callback/logout marker %s', search => {
    expect(claimSilentSSO('/app/' + search, search)).toBeUndefined()
    expect(window.sessionStorage.getItem('postra.sso.silentAttempted')).toBeNull()
  })

  it.each(['postra.sso.silentAttempted', 'postra.sso.signedOut'])('honors the existing legacy %s marker', key => {
    window.sessionStorage.setItem(key, 'true')
    expect(claimSilentSSO('/app/', '')).toBeUndefined()
  })

  it('fails closed when session storage cannot be read', () => {
    vi.spyOn(window, 'sessionStorage', 'get').mockImplementation(() => { throw new DOMException('Denied', 'SecurityError') })
    expect(claimSilentSSO('/app/', '')).toBeUndefined()
  })

  it('fails closed when the attempt marker cannot be written', () => {
    vi.spyOn(Storage.prototype, 'setItem').mockImplementation(() => { throw new DOMException('Denied', 'SecurityError') })
    expect(claimSilentSSO('/app/', '')).toBeUndefined()
  })

  it('fails closed when browser storage silently ignores a write', () => {
    vi.spyOn(Storage.prototype, 'setItem').mockImplementation(() => {})
    expect(claimSilentSSO('/app/', '')).toBeUndefined()
  })

  it('clears both legacy guards after a successful login', () => {
    window.sessionStorage.setItem('postra.sso.silentAttempted', 'true')
    markSignedOut()
    resetSSOFlags()
    expect(window.sessionStorage.getItem('postra.sso.silentAttempted')).toBeNull()
    expect(window.sessionStorage.getItem('postra.sso.signedOut')).toBeNull()
    expect(claimSilentSSO('/app/', '')).toBeDefined()
  })

  it.each(['https://evil.example/app/', '//evil.example/app/', '/ui/', '/app/../../outside', '/app\\evil'])('does not accept a non-SPA return destination %s', path => {
    expect(claimSilentSSO(path, '')).toBeUndefined()
  })

  it('blocks auto login in another tab after a deliberate logout event', () => {
    expect(receiveAuthChange(new StorageEvent('storage', { key: 'postra-auth-change', newValue: '123:signed_out' }))).toBe(true)
    expect(window.sessionStorage.getItem('postra.sso.signedOut')).toBe('true')
    expect(claimSilentSSO('/app/', '')).toBeUndefined()
    // A subsequent ordinary session notification cannot undo explicit logout.
    receiveAuthChange(new StorageEvent('storage', { key: 'postra-auth-change', newValue: '124' }))
    expect(claimSilentSSO('/app/', '')).toBeUndefined()
  })

  it('retains the cross-tab logout guard even when storing it fails', () => {
    const write = vi.spyOn(Storage.prototype, 'setItem').mockImplementation(() => { throw new DOMException('Denied', 'SecurityError') })
    expect(() => receiveAuthChange(new StorageEvent('storage', { key: 'postra-auth-change', newValue: '123:signed_out' }))).not.toThrow()
    write.mockRestore()
    expect(claimSilentSSO('/app/', '')).toBeUndefined()
  })

  it('ignores unrelated storage events', () => {
    expect(receiveAuthChange(new StorageEvent('storage', { key: 'unrelated', newValue: '123:signed_out' }))).toBe(false)
    expect(claimSilentSSO('/app/', '')).toBeDefined()
  })
})

describe('standalone auth deep links', () => {
  it('retains the requested SPA route through the login screen', () => {
    expect(authReturnTo('/login', '?return_to=%2Fapp%2Fmessages%2Fm1%3Fbody%3Dtrue')).toBe('/app/messages/m1?body=true')
    expect(authReturnTo('/mail', '?folder=important')).toBe('/app/mail?folder=important')
  })
  it.each(['https://evil.example', '//evil.example/app/', '/ui/', '/app/../../outside', '/app/login', '/app/setup'])('rejects an external or recursive return destination %s', returnTo => {
    expect(authReturnTo('/login', '?return_to=' + encodeURIComponent(returnTo))).toBe('/app/')
  })
})
