import {useState, type FormEvent, type ReactNode} from 'react'
import {Link, useLocation} from 'react-router-dom'
import {useQuery, useQueryClient} from '@tanstack/react-query'
import {ChevronDown, LockKeyhole, Mail, ShieldCheck} from 'lucide-react'
import {api} from '@/api/client'
import {Button, ErrorState, Input, Loading} from '@/components/ui'
import {notifyAuthChange, resetSSOFlags} from '@/lib/auth-events'
import type {Principal} from '@/app/session'
import './auth.css'

export type BrowserSession = {authenticated: boolean; auth_enabled: boolean; principal?: Principal; login_url: string; local_auth?: boolean; token_required?: boolean; oidc_url?: string; oidc_auto_login?: boolean; setup_url?: string; sso_error?: string}

function AuthFrame({children}: {children: ReactNode}) {
  return <div className="auth-page"><section className="login-panel"><span className="brand-mark"><Mail size={30}/></span>{children}</section></div>
}

export function AuthErrorPage({error, retry}: {error?: unknown; retry?: () => void}) {
  return <AuthFrame><h1>화면을 열지 못했습니다</h1><ErrorState error={error ?? new Error('요청을 처리하지 못했습니다. 다시 로그인하거나 잠시 후 시도하세요.')} retry={retry}/><Button variant="outline" asChild><Link to="/login">로그인 화면으로</Link></Button></AuthFrame>
}

// Organisation (SSO) sign-in leads when it is configured, as in the internal
// app store: one primary action, with the local form folded behind a toggle.
// Without SSO the local form is the only method and shows at once.
export function LoginPage({session, returnTo}: {session: BrowserSession; returnTo: string}) {
  const cache = useQueryClient()
  const location = useLocation()
  const [loginID, setLoginID] = useState('')
  const [password, setPassword] = useState('')
  const [token, setToken] = useState('')
  const [pending, setPending] = useState(false)
  const [error, setError] = useState<unknown>()
  const [localOpen, setLocalOpen] = useState(false)
  const oidcURL = session.oidc_url ? new URL(session.oidc_url, window.location.origin) : undefined
  oidcURL?.searchParams.set('return_to', returnTo)
  const sso = oidcURL !== undefined
  // Absent local_auth means available, as before: only an explicit false hides it.
  const localAvailable = session.token_required === true || session.local_auth !== false
  const localShown = localAvailable && (!sso || localOpen)
  // The callback sends a refused prompt=none attempt here with sso=none. The
  // marker only explains the screen; it never changes where the buttons go.
  const ssoRefused = sso && new URLSearchParams(location.search).get('sso') === 'none'
  async function submit(event: FormEvent) {
    event.preventDefault(); if (pending) return
    setError(undefined); setPending(true)
    try {
      const result = await api<{redirect_url: string}>('/auth/login', {body: session.token_required ? {token, return_to: returnTo} : {login_id: loginID, password, return_to: returnTo}})
      setPassword(''); setToken(''); resetSSOFlags(); notifyAuthChange()
      await cache.cancelQueries(); cache.clear()
      window.location.assign(result.redirect_url)
    } catch (error) {setError(error); setPending(false)}
  }
  return <AuthFrame><h1>메일에서, 다음 할 일까지.</h1><p className="muted">{sso ? '조직 계정으로 로그인하고 나만의 업무 공간을 시작하세요.' : '계정으로 로그인하고 나만의 업무 공간을 시작하세요.'}</p>
    {session.sso_error && <ErrorState error={new Error(session.sso_error)}/>}{error != null && <ErrorState error={error}/>}
    {session.setup_url ? <><p className="muted">첫 관리자를 등록하면 워크스페이스를 사용할 수 있습니다.</p><Button asChild><Link to="/setup">최초 관리자 설정</Link></Button></> : <div className="auth-methods">
      {ssoRefused && <div className="auth-notice" role="status"><ShieldCheck size={18}/><span>조직 계정 세션이 없어 자동으로 로그인하지 않았습니다. 아래 버튼으로 로그인하세요.</span></div>}
      {oidcURL && <Button asChild className="auth-sso"><a href={oidcURL.pathname + oidcURL.search}><ShieldCheck size={18}/>조직 계정으로 로그인</a></Button>}
      {sso && localAvailable && <button type="button" className="local-login-toggle" aria-expanded={localOpen} aria-controls="local-login-form" onClick={() => setLocalOpen(open => !open)}>
        <LockKeyhole size={16}/><span>{session.token_required ? '접속 토큰으로 로그인' : '로컬 계정으로 로그인'}</span><ChevronDown size={16}/>
      </button>}
      {localShown && <form id="local-login-form" className="auth-form" onSubmit={submit}>
        {sso && <div className="auth-notice"><LockKeyhole size={18}/><span>조직 계정이 없거나 사용할 수 없을 때 Postra 로컬 계정으로 로그인합니다.</span></div>}
        {session.token_required ? <label>접속 토큰<Input type="password" value={token} onChange={event => setToken(event.target.value)} autoComplete="current-password" required disabled={pending}/></label> : <>
          <label>로그인 ID<Input name="login_id" value={loginID} onChange={event => setLoginID(event.target.value)} autoComplete="username" required disabled={pending}/></label>
          <label>비밀번호<Input name="password" type="password" value={password} onChange={event => setPassword(event.target.value)} autoComplete="current-password" required disabled={pending}/></label>
        </>}
        <Button type="submit" variant={sso ? 'outline' : 'default'} disabled={pending}><ShieldCheck size={17}/>{pending ? '로그인 중…' : '계정으로 로그인'}</Button>
      </form>}
      {!sso && !localAvailable && <div className="auth-notice auth-notice-danger" role="alert"><span>사용 가능한 로그인 방식이 없습니다. 관리자에게 문의하세요.</span></div>}
    </div>}
    <p className="small muted">메일은 본인 계정만 표시됩니다.</p>
  </AuthFrame>
}

type SetupState = {required: boolean; allowed: boolean; login_url: string}
export function SetupPage() {
  const cache = useQueryClient()
  const state = useQuery({queryKey: ['auth-setup'], queryFn: () => api<SetupState>('/auth/setup'), retry: false})
  const [loginID, setLoginID] = useState('')
  const [displayName, setDisplayName] = useState('')
  const [password, setPassword] = useState('')
  const [confirmation, setConfirmation] = useState('')
  const [pending, setPending] = useState(false)
  const [error, setError] = useState<unknown>()
  async function submit(event: FormEvent) {
    event.preventDefault(); if (pending) return
    if (password !== confirmation) {setError(new Error('비밀번호 확인이 일치하지 않습니다.')); return}
    setPending(true); setError(undefined)
    try {
      const result = await api<{redirect_url: string}>('/auth/setup', {body: {login_id: loginID, display_name: displayName, password}})
      setPassword(''); setConfirmation(''); resetSSOFlags(); notifyAuthChange()
      await cache.cancelQueries(); cache.clear(); window.location.assign(result.redirect_url)
    } catch (error) {setError(error); setPending(false)}
  }
  return <AuthFrame><h1>최초 관리자 설정</h1><p className="muted">조직의 메일 워크스페이스를 관리할 계정을 등록하세요.</p>
    {state.isPending ? <Loading/> : state.error ? <ErrorState error={state.error} retry={() => state.refetch()}/> : !state.data?.required ? <><p>최초 관리자 설정이 완료되어 있습니다.</p><Button asChild><Link to="/login">로그인 화면으로</Link></Button></> : !state.data.allowed ? <ErrorState error={new Error('서버의 로컬 주소에서 초기 설정하거나 운영 관리자가 부트스트랩 관리자 비밀번호를 구성해야 합니다.')}/> : <form className="auth-form" onSubmit={submit}>
      {error != null && <ErrorState error={error}/>}
      <label>로그인 ID<Input value={loginID} onChange={event => setLoginID(event.target.value)} autoComplete="username" required disabled={pending}/></label>
      <label>표시 이름<Input value={displayName} onChange={event => setDisplayName(event.target.value)} autoComplete="name" required disabled={pending}/></label>
      <label>비밀번호<Input aria-label="비밀번호" aria-describedby="setup-password-hint" type="password" value={password} onChange={event => setPassword(event.target.value)} minLength={12} autoComplete="new-password" required disabled={pending}/><small id="setup-password-hint" className="muted">12자 이상 입력하세요.</small></label>
      <label>비밀번호 확인<Input type="password" value={confirmation} onChange={event => setConfirmation(event.target.value)} minLength={12} autoComplete="new-password" required disabled={pending}/></label>
      <Button type="submit" disabled={pending}>{pending ? '설정 중…' : '관리자 계정 만들기'}</Button>
    </form>}
  </AuthFrame>
}
