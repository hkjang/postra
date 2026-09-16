import {InvalidResponseError} from './response'

export class APIError extends Error {
  constructor(message: string, public status: number, public code?: string, public traceID?: string) { super(message); this.name = 'APIError' }
}
type Options = {method?: string; body?: unknown; signal?: AbortSignal}

export function csrfToken() {
  const cookie = document.cookie.split(';').map(s => s.trim()).find(s => s.startsWith('postra_csrf='))
  return cookie ? decodeURIComponent(cookie.slice('postra_csrf='.length)) : ''
}

// Browser identity is an HttpOnly session cookie. Never persist API keys or
// bearer tokens in localStorage. The only script-readable token is CSRF.
export async function api<T>(path: string, options: Options = {}): Promise<T> {
  if (!path.startsWith('/api/') && !path.startsWith('/auth/')) throw new Error('API 경로가 올바르지 않습니다.')
  const method = options.method ?? (options.body === undefined ? 'GET' : 'POST')
  const headers: Record<string, string> = {Accept: 'application/json'}
  if (options.body !== undefined) headers['Content-Type'] = 'application/json'
  if (!['GET', 'HEAD', 'OPTIONS'].includes(method.toUpperCase())) headers['X-CSRF-Token'] = csrfToken()
  const response = await fetch(path, {method, headers, credentials: 'same-origin', signal: options.signal,
    ...(options.body !== undefined ? {body: JSON.stringify(options.body)} : {})})
  const text = response.status === 204 ? '' : await response.text()
  let result: unknown
  try { result = text ? JSON.parse(text) : undefined } catch { result = undefined }
  if (!response.ok) {
    // No request body, credentials, or upstream HTML is placed in exceptions.
    const error = result && typeof result === 'object' && 'message' in result ? String(result.message) : result && typeof result === 'object' && 'error' in result ? String(result.error) : `요청을 처리하지 못했습니다 (${response.status}).`
    // App already handles session-query failures. Rebroadcasting that same
    // query's 401 would cancel/refetch itself indefinitely.
    if (response.status === 401 && !/^\/auth\/(?:login|setup|session)(?:\?|$)/.test(path)) window.dispatchEvent(new Event('postra:unauthorized'))
    const contract = result && typeof result === 'object' ? result as Record<string, unknown> : {}
    const code = typeof contract.code === 'string' ? contract.code.slice(0, 120) : undefined
    const traceID = typeof contract.trace_id === 'string' ? contract.trace_id.slice(0, 128) : undefined
    throw new APIError(error, response.status, code, traceID)
  }
  // An HTML proxy/login page or malformed 200 is not a successful empty result.
  // Keep 204 for mutations with no response body, and valid JSON null for DTOs
  // which explicitly allow it. Never include the received body in the error.
  if (response.status !== 204 && result === undefined) throw new InvalidResponseError()
  return result as T
}
