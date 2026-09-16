import {lazy, Suspense, useEffect, useState} from 'react'
import {Navigate, Route, Routes, useLocation} from 'react-router-dom'
import {useQuery, useQueryClient} from '@tanstack/react-query'
import {api, APIError} from '@/api/client'
import {InvalidResponseError} from '@/api/response'
import {SessionContext} from './session'
import {UserDataProvider} from './providers'
import {Workspace} from '@/components/layout/Workspace'
import {Button, EmptyState, Loading} from '@/components/ui'
import {authReturnTo, claimSilentSSO, markSignedOut, notifyAuthChange, receiveAuthChange, resetSSOFlags} from '@/lib/auth-events'
import {AuthErrorPage, LoginPage, SetupPage} from '@/features/auth'
import {parseBrowserSession} from '@/features/auth/response'
import {BrowserTracking} from '@/features/tracking'
const InboxPage = lazy(() => import('@/features/inbox/InboxPage').then(m => ({default: m.InboxPage})))
const MessagePage = lazy(() => import('@/features/messages/MessagePage').then(m => ({default: m.MessagePage})))
const ThreadPage = lazy(() => import('@/features/messages/ThreadPage').then(m => ({default: m.ThreadPage})))
const JobsPage = lazy(() => import('@/features/jobs').then(m => ({default: m.JobsPage})))
const PersonalSettingsPage = lazy(() => import('@/features/settings').then(m => ({default: m.PersonalSettingsPage})))
const AccountPreferencesPage = lazy(() => import('@/features/settings').then(m => ({default: m.AccountPreferencesPage})))
const SignaturesPage = lazy(() => import('@/features/compose/SignaturesPage').then(m => ({default: m.SignaturesPage})))
const TrackingPage = lazy(() => import('@/features/tracking').then(m => ({default: m.TrackingPage})))
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
const MCPKeysPage = lazy(() => import('@/features/mcp').then(m => ({default: m.MCPKeysPage})))

export function App() {
  const cache = useQueryClient()
  const location = useLocation()
  const [identityRevoked, setIdentityRevoked] = useState(false)
  const session = useQuery({queryKey: ['session'], queryFn: async ({signal}) => parseBrowserSession(await api('/auth/session' + (new URLSearchParams(location.search).get('sso') === 'error' ? '?sso=error' : ''), {signal})), retry: false, staleTime: 0, refetchOnWindowFocus: 'always', refetchInterval: 30000})
  useEffect(() => {
    if (session.error instanceof APIError && session.error.status >= 400 && session.error.status < 500) setIdentityRevoked(true)
    else if (session.isSuccess && !session.isFetching) setIdentityRevoked(false)
  }, [session.error, session.isSuccess, session.isFetching, session.dataUpdatedAt])
  useEffect(() => {
    const invalidate = (revokeIdentity = false) => {
      void cache.cancelQueries()
      // An explicit 401/logout is not a transient connectivity failure. Drop
      // the authenticated identity immediately, even if revalidation then
      // fails or hangs; unmounting also discards the private provider/editor.
      if (revokeIdentity) cache.setQueryData<ReturnType<typeof parseBrowserSession>>(['session'], current => current && ({...current, authenticated: false, principal: undefined, oidc_auto_login: false}))
      cache.removeQueries({predicate: query => query.queryKey[0] !== 'session'})
      void cache.invalidateQueries({queryKey: ['session']})
    }
    const unauthorized = () => invalidate(true)
    window.addEventListener('postra:unauthorized', unauthorized)
    const changed = (event: StorageEvent) => {if (receiveAuthChange(event)) invalidate(/^\d+:signed_out$/.test(event.newValue || ''))}
    window.addEventListener('storage', changed)
    return () => {window.removeEventListener('postra:unauthorized', unauthorized); window.removeEventListener('storage', changed)}
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
    const target = claimSilentSSO(authReturnTo(location.pathname, location.search), location.search)
    if (target) window.location.assign(target)
  }, [session.data?.authenticated, session.data?.principal?.user_id, session.data?.oidc_auto_login, session.data?.oidc_url, session.data?.setup_url, session.error, location.pathname, location.search])
  // A background refresh is not a navigation. Keep the existing identity and
  // mounted editor on transport/5xx errors; unmounting it would silently lose
  // unsaved mail. Explicit unauthenticated data and 4xx failures still remove
  // the private workspace immediately.
  const transientFailure = session.error instanceof TypeError || session.error instanceof InvalidResponseError ||
    (session.error instanceof APIError && session.error.status >= 500 && session.error.status <= 599) ||
    (session.error instanceof DOMException && ['NetworkError', 'TimeoutError'].includes(session.error.name))
  // A 5xx after a prior 401 must not revive the identity from stale query data.
  const retainWorkspace = !identityRevoked && session.data?.authenticated === true && transientFailure
  if (session.isPending) return <div className="auth-page"><Loading label="내 워크스페이스를 여는 중…"/></div>
  if (session.error && !retainWorkspace) return <AuthErrorPage error={session.error} retry={() => session.refetch()}/>
  if (location.pathname === '/error') return <AuthErrorPage/>
  if (!session.data?.authenticated) {
    if (location.pathname === '/setup') return <SetupPage/>
    return <LoginPage session={session.data!} returnTo={authReturnTo(location.pathname, location.search)}/>
  }
  if (['/login', '/setup'].includes(location.pathname)) return <Navigate replace to={authReturnTo(location.pathname, location.search).replace(/^\/app/, '') || '/'}/>
  return <UserDataProvider key={session.data.principal?.user_id}><SessionContext.Provider value={session.data.principal}>
    <BrowserTracking/>
    {retainWorkspace && <div role="status" style={{position: 'fixed', bottom: 16, right: 16, zIndex: 100, maxWidth: 'calc(100vw - 32px)', padding: '12px 16px', border: '1px solid var(--border)', borderRadius: 8, background: 'var(--surface)', boxShadow: '0 4px 20px #0002', fontSize: 12}}><p style={{margin: '0 0 8px'}}>서버 연결을 확인하지 못했습니다. 작성 중인 내용은 유지됩니다.</p><Button size="sm" variant="outline" disabled={session.isFetching} onClick={() => session.refetch()}>{session.isFetching ? '연결 확인 중…' : '연결 다시 확인'}</Button></div>}
    <Suspense fallback={<Loading/>}><Routes><Route element={<Workspace/>}>
    <Route path="keys" element={<MCPKeysPage/>}/>
    <Route path="threads/:id" element={<ThreadPage/>}/><Route path="jobs" element={<JobsPage/>}/><Route path="jobs/:id" element={<JobsPage/>}/>
    <Route path="settings" element={<PersonalSettingsPage/>}/><Route path="accounts/:id/preferences" element={<AccountPreferencesPage/>}/><Route path="settings/signatures" element={<SignaturesPage/>}/>
    <Route path="admin/tracking" element={session.data.principal?.role === 'admin' ? <TrackingPage/> : <EmptyState title="관리자 권한이 필요합니다"/>}/>
    <Route index element={<Navigate replace to="/mail"/>}/><Route path="mail" element={<InboxPage/>}/><Route path="mail/:id" element={<InboxPage/>}/><Route path="search" element={<InboxPage/>}/><Route path="messages/:id" element={<MessagePage/>}/><Route path="compose" element={<ComposePage/>}/><Route path="drafts/:id" element={<ComposePage/>}/><Route path="drafts" element={<DraftsPage/>}/><Route path="sent" element={<SentPage/>}/><Route path="work" element={<WorkPage/>}/><Route path="team" element={<TeamPage/>}/><Route path="actions" element={<ActionsPage/>}/><Route path="ask" element={<AskPage/>}/><Route path="digest" element={<DigestPage/>}/><Route path="rules" element={<RulesPage/>}/><Route path="accounts" element={<AccountsPage/>}/><Route path="accounts/:id" element={<AccountDetailPage/>}/><Route path="admin/*" element={session.data.principal?.role === 'admin' ? <AdminPage/> : <EmptyState title="관리자 권한이 필요합니다"/>}/><Route path="*" element={<EmptyState title="페이지를 찾을 수 없습니다" action={<Button asChild><a href="/app/mail">받은메일로</a></Button>}/>}/>
  </Route></Routes></Suspense></SessionContext.Provider></UserDataProvider>
}
