// Cross-tab notification is best effort, not an authentication requirement.
// Storage can be disabled by browser policy or private-mode restrictions;
// session focus refresh and polling remain the authoritative fallback.
export function notifyAuthChange(kind?: 'signed_out'): void {
  try {
    window.localStorage.setItem('postra-auth-change', String(Date.now()) + (kind === 'signed_out' ? ':signed_out' : ''))
  } catch {
    // No credentials or mail are stored, and denied storage must never break
    // login, logout, or the authenticated workspace.
  }
}

// The two guards need different lifetimes, so they live in different storages.
//
// "Already attempted" belongs to one tab: a fresh tab should try again, while
// a reload after a refusal must not bounce, so it is in sessionStorage.
//
// "Signed out" is a decision about this browser. Keycloak's session outlives
// Postra's (logout does not end it), so a per-tab marker let the very next tab
// — or the same one reopened — sign the user straight back in through
// prompt=none: logout did not stick. It is in localStorage and is lifted only
// when a session exists again. The legacy per-tab marker is still honoured.
const attemptedKey = 'postra.sso.silentAttempted'
const signedOutKey = 'postra.sso.signedOut'
let claimedInThisPage = false
let signedOutInThisPage = false

export function markSignedOut(): void {
  signedOutInThisPage = true
  try { window.localStorage.setItem(signedOutKey, 'true') } catch { /* URL marker and fail-closed storage guards remain. */ }
}

// Match the legacy authenticated layout: only successful login clears the
// failed-attempt and deliberate-signout guards.
export function resetSSOFlags(): void {
  claimedInThisPage = false
  signedOutInThisPage = false
  try { window.localStorage.removeItem(signedOutKey) } catch { /* Storage denial must never break a successful login. */ }
  try {
    window.sessionStorage.removeItem(attemptedKey)
    window.sessionStorage.removeItem(signedOutKey)
  } catch { /* Storage denial must never break a successful login. */ }
}

// Screens that never start a silent attempt. A person who opened the login
// screen asked to see it, and the error and setup screens are where a loop
// would come from. Paths are relative to the /app router base.
export function isSilentSSOScreen(pathname: string): boolean {
  return !['/login', '/setup', '/error'].includes(pathname)
}

// Returns a navigation target only after atomically claiming this tab's one
// silent attempt. A second effect (including StrictMode) cannot claim it again.
export function claimSilentSSO(returnTo: string, search: string): string | undefined {
  if (new URLSearchParams(search).has('sso') || claimedInThisPage || signedOutInThisPage) return
  // Only the current local SPA may be a return target, never an arbitrary URL.
  if (!/^\/app(?:\/|\?|$)/.test(returnTo) || /[\\\u0000-\u001f\u007f]/.test(returnTo)) return
  const target = new URL(returnTo, window.location.origin)
  if (target.origin !== window.location.origin || !/^\/app(?:\/|$)/.test(target.pathname)) return
  try {
    if (window.localStorage.getItem(signedOutKey) === 'true') return
    const storage = window.sessionStorage
    if (storage.getItem(signedOutKey) === 'true' || storage.getItem(attemptedKey) === 'true') return
    claimedInThisPage = true
    storage.setItem(attemptedKey, 'true')
    // Some privacy shims silently ignore writes. Do not navigate unless the
    // loop-prevention marker is actually readable after writing it.
    if (storage.getItem(attemptedKey) !== 'true') return
  } catch { return }
  return '/auth/oidc/start?prompt=none&return_to=' + encodeURIComponent(returnTo)
}

// Keep deep links within the SPA and avoid returning to an authentication
// screen after a successful login. Never use a supplied absolute URL.
export function authReturnTo(pathname: string, search: string): string {
  const requested = ['/login', '/setup', '/error'].includes(pathname)
    ? new URLSearchParams(search).get('return_to') || '/app/'
    : '/app' + pathname + search
  if (!/^\/app(?:\/|\?|$)/.test(requested) || /[\\\u0000-\u001f\u007f]/.test(requested)) return '/app/'
  try {
    const target = new URL(requested, window.location.origin)
    if (target.origin !== window.location.origin || !/^\/app(?:\/|$)/.test(target.pathname) ||
      ['/app/login', '/app/setup', '/app/error'].includes(target.pathname)) return '/app/'
    return target.pathname + target.search + target.hash
  } catch { return '/app/' }
}

// The marker contains no identity or credential. Other tabs must remember a
// deliberate logout before revalidating the session, or auto SSO could undo it.
export function receiveAuthChange(event: StorageEvent): boolean {
  if (event.key !== 'postra-auth-change') return false
  if (/^\d+:signed_out$/.test(event.newValue || '')) markSignedOut()
  return true
}
