import {lazy, Suspense, useEffect} from 'react'
import {Navigate, Route, Routes, useLocation} from 'react-router-dom'
import {useQuery, useQueryClient} from '@tanstack/react-query'
import {Mail, ShieldCheck} from 'lucide-react'
import {api, APIError} from '@/api/client'
import {SessionContext, type Principal} from './session'
import {UserDataProvider} from './providers'
import {Workspace} from '@/components/layout/Workspace'
import {Button, EmptyState, ErrorState, Loading} from '@/components/ui'
import {claimSilentSSO, markSignedOut, notifyAuthChange, receiveAuthChange, resetSSOFlags} from '@/lib/auth-events'
const InboxPage = lazy(() => import('@/features/inbox/InboxPage').then(m => ({default: m.InboxPage})))
const MessagePage = lazy(() => import('@/features/messages/MessagePage').then(m => ({default: m.MessagePage})))
const ComposePage = lazy(() => import('@/features/compose/ComposePage').then(m => ({default: m.ComposePage})))
const WorkPage = lazy(() => import('@/features/work').then(m => ({default: m.WorkPage})))
const TeamPage = lazy(() => import('@/features/work').then(m => ({default: m.TeamPage})))
const ActionsPage = lazy(() => import('@/features/actions').then(m => ({default: m.ActionsPage})))
const AskPage = lazy(() => import('@/features/ai').then(m => ({default: m.AskPage})))
const DigestPage = lazy(() => import('@/features/ai').then(m => ({default: m.DigestPage})))
const SentPage = lazy(() => import('@/features/sent').then(m => ({default: m.SentPage})))
const DraftsPage = lazy(() => import('@/features/drafts').then(m => ({default: m.DraftsPage})))
const RulesPage = lazy(() => import('@/features/rules').then(m => ({default: m.RulesPage})))
const AccountsPage = lazy(() => import('@/features/accounts').then(m => ({default: m.AccountsPage})))
const AccountDetailPage = lazy(() => import('@/features/accounts').then(m => ({default: m.AccountDetailPage})))
const AdminPage = lazy(() => import('@/features/admin').then(m => ({default: m.AdminPage})))
const MCPKeysPage = lazy(() => import('@/features/admin').then(m => ({default: m.MCPKeysPage})))
type Session = {authenticated: boolean; auth_enabled: boolean; principal?: Principal; login_url: string; oidc_url?: string; oidc_auto_login?: boolean; setup_url?: string}

export function App() {
  const cache = useQueryClient()
  const location = useLocation()
  const session = useQuery({queryKey: ['session'], queryFn: () => api<Session>('/api/auth/session'), retry: false, staleTime: 0, refetchOnWindowFocus: 'always', refetchInterval: 30000})
  useEffect(() => {
    const invalidate = () => {void cache.cancelQueries(); cache.removeQueries({predicate: query => query.queryKey[0] !== 'session'}); void cache.invalidateQueries({queryKey: ['session']})}
    window.addEventListener('postra:unauthorized', invalidate)
    const changed = (event: StorageEvent) => {if (receiveAuthChange(event)) invalidate()}
    window.addEventListener('storage', changed)
    return () => {window.removeEventListener('postra:unauthorized', invalidate); window.removeEventListener('storage', changed)}
  }, [cache])
  useEffect(() => {
    // Non-sensitive notification only: never put credentials or mail in storage.
    if (session.data) notifyAuthChange()
  }, [session.data?.principal?.user_id, session.data?.authenticated])
  useEffect(() => {
    if (session.data?.authenticated) resetSSOFlags()
  }, [session.data?.authenticated, session.data?.principal?.user_id])
  useEffect(() => {
    if (session.data?.authenticated) return
    const params = new URLSearchParams(location.search)
    if (params.get('sso') === 'signed_out') markSignedOut()
    if (session.error || session.data?.authenticated !== false || !session.data.oidc_auto_login || !session.data.oidc_url || session.data.setup_url) return
    const target = claimSilentSSO('/app' + location.pathname + location.search, location.search)
    if (target) window.location.assign(target)
  }, [session.data?.authenticated, session.data?.principal?.user_id, session.data?.oidc_auto_login, session.data?.oidc_url, session.data?.setup_url, session.error, location.pathname, location.search])
  // A background refresh is not a navigation. Keep the existing identity and
  // mounted editor on transport/5xx errors; unmounting it would silently lose
  // unsaved mail. Explicit unauthenticated data and 4xx failures still remove
  // the private workspace immediately.
  const transientFailure = session.error instanceof TypeError ||
    (session.error instanceof APIError && session.error.status >= 500 && session.error.status <= 599) ||
    (session.error instanceof DOMException && ['NetworkError', 'TimeoutError'].includes(session.error.name))
  const retainWorkspace = session.data?.authenticated === true && transientFailure
  if (session.isPending) return <div className="auth-page"><Loading label="내 워크스페이스를 여는 중…"/></div>
  if (session.error && !retainWorkspace) return <div className="auth-page"><ErrorState error={session.error} retry={() => session.refetch()}/><a href="/ui/">기존 UI 열기</a></div>
  if (!session.data?.authenticated) {
    const returnTo = '/app' + location.pathname + location.search
    const loginURL = '/ui/login?return_to=' + encodeURIComponent(returnTo)
    const oidcURL = session.data?.oidc_url ? new URL(session.data.oidc_url, window.location.origin) : undefined
    oidcURL?.searchParams.set('return_to', returnTo)
    return <div className="auth-page"><div className="login-panel"><span className="brand-mark"><Mail size={30}/></span><h1>메일에서, 다음 할 일까지.</h1><p className="muted">Postra AI Work Mail에 오신 것을 환영합니다.<br/>회사 계정으로 로그인하고 나만의 업무 공간을 시작하세요.</p>{session.data?.setup_url ? <Button asChild><a href={session.data.setup_url}>최초 관리자 설정</a></Button> : <><Button asChild><a href={loginURL}><ShieldCheck size={17}/>계정으로 로그인</a></Button>{oidcURL && <Button variant="outline" asChild><a href={oidcURL.pathname + oidcURL.search}>회사 SSO로 로그인</a></Button>}</>}<p className="small muted">기존 계정과 SSO를 사용합니다. 메일은 본인 계정만 표시됩니다.</p></div></div>
  }
  return <UserDataProvider key={session.data.principal?.user_id}><SessionContext.Provider value={session.data.principal}>
    {retainWorkspace && <div role="status" style={{position: 'fixed', bottom: 16, right: 16, zIndex: 100, maxWidth: 'calc(100vw - 32px)', padding: '12px 16px', border: '1px solid var(--border)', borderRadius: 8, background: 'var(--surface)', boxShadow: '0 4px 20px #0002', fontSize: 12}}><p style={{margin: '0 0 8px'}}>서버 연결을 확인하지 못했습니다. 작성 중인 내용은 유지됩니다.</p><Button size="sm" variant="outline" disabled={session.isFetching} onClick={() => session.refetch()}>{session.isFetching ? '연결 확인 중…' : '연결 다시 확인'}</Button></div>}
    <Suspense fallback={<Loading/>}><Routes><Route element={<Workspace/>}>
    <Route path="keys" element={<MCPKeysPage/>}/>
    <Route index element={<Navigate replace to="/mail"/>}/><Route path="mail" element={<InboxPage/>}/><Route path="mail/:id" element={<InboxPage/>}/><Route path="search" element={<InboxPage/>}/><Route path="messages/:id" element={<MessagePage/>}/><Route path="compose" element={<ComposePage/>}/><Route path="drafts/:id" element={<ComposePage/>}/><Route path="drafts" element={<DraftsPage/>}/><Route path="sent" element={<SentPage/>}/><Route path="work" element={<WorkPage/>}/><Route path="team" element={<TeamPage/>}/><Route path="actions" element={<ActionsPage/>}/><Route path="ask" element={<AskPage/>}/><Route path="digest" element={<DigestPage/>}/><Route path="rules" element={<RulesPage/>}/><Route path="accounts" element={<AccountsPage/>}/><Route path="accounts/:id" element={<AccountDetailPage/>}/><Route path="admin/*" element={session.data.principal?.role === 'admin' ? <AdminPage/> : <EmptyState title="관리자 권한이 필요합니다"/>}/><Route path="*" element={<EmptyState title="페이지를 찾을 수 없습니다" action={<Button asChild><a href="/app/mail">받은메일로</a></Button>}/>}/>
  </Route></Routes></Suspense></SessionContext.Provider></UserDataProvider>
}
