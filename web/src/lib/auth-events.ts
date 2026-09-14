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

const attemptedKey = 'postra.sso.silentAttempted'
const signedOutKey = 'postra.sso.signedOut'
let claimedInThisPage = false
let signedOutInThisPage = false

export function markSignedOut(): void {
  signedOutInThisPage = true
  try { window.sessionStorage.setItem(signedOutKey, 'true') } catch { /* URL marker and fail-closed storage guards remain. */ }
}

// Match the legacy authenticated layout: only successful login clears the
// tab's failed-attempt and deliberate-signout guards.
export function resetSSOFlags(): void {
  claimedInThisPage = false
  signedOutInThisPage = false
  try {
    window.sessionStorage.removeItem(attemptedKey)
    window.sessionStorage.removeItem(signedOutKey)
  } catch { /* Storage denial must never break a successful login. */ }
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
    const storage = window.sessionStorage
    if (storage.getItem(signedOutKey) === 'true' || storage.getItem(attemptedKey) === 'true') return
    claimedInThisPage = true
    storage.setItem(attemptedKey, 'true')
    // Some privacy shims silently ignore writes. Do not navigate unless the
    // loop-prevention marker is actually readable after writing it.
    if (storage.getItem(attemptedKey) !== 'true') return
  } catch { return }
  return '/ui/auth/oidc/start?prompt=none&return_to=' + encodeURIComponent(returnTo)
}

// The marker contains no identity or credential. Other tabs must remember a
// deliberate logout before revalidating the session, or auto SSO could undo it.
export function receiveAuthChange(event: StorageEvent): boolean {
  if (event.key !== 'postra-auth-change') return false
  if (/^\d+:signed_out$/.test(event.newValue || '')) markSignedOut()
  return true
}
