import {useEffect, useState} from 'react'
import {Link, NavLink, Outlet, useLocation, useNavigate} from 'react-router-dom'
import {useQuery, useQueryClient} from '@tanstack/react-query'
import {Command} from 'cmdk'
import * as Dialog from '@radix-ui/react-dialog'
import {Bell, BriefcaseBusiness, CheckSquare, ChevronDown, Command as CommandIcon, FilePenLine, Inbox, LayoutList, LogOut, Mail, Menu, Moon, Plus, Search, Settings, ShieldCheck, Sparkles, Star, Sun, Users, X} from 'lucide-react'
import {toast} from 'sonner'
import {api} from '@/api/client'
import {useSession} from '@/app/session'
import {Button, ErrorState, Loading} from '@/components/ui'
import {usePreferences} from '@/stores/preferences'
import {formatDate} from '@/lib/utils'
import {markSignedOut, notifyAuthChange} from '@/lib/auth-events'

const navigation = [
  {label: 'MAIL', items: [{to: '/mail', label: '받은메일', icon: Inbox}, {to: '/mail?folder=important', label: '중요 메일', icon: Star}, {to: '/sent', label: '발송 메일', icon: Mail}, {to: '/drafts', label: '초안', icon: FilePenLine}]},
  {label: 'WORK', items: [{to: '/work', label: '내 업무', icon: BriefcaseBusiness}, {to: '/team', label: '팀 업무함', icon: Users}, {to: '/actions', label: '액션 센터', icon: CheckSquare}]},
  {label: 'AI', items: [{to: '/ask', label: 'Ask Postra', icon: Sparkles}, {to: '/digest', label: '브리핑', icon: LayoutList}]},
  {label: 'WORKSPACE', items: [{to: '/accounts', label: '메일 계정', icon: Mail}, {to: '/rules', label: '자동화 규칙', icon: Settings}, {to: '/keys', label: 'MCP 연결 키', icon: ShieldCheck}]},
]
type Job = {id: string; type: string; status: string; created_at: number; error?: string; error_message?: string}

export function Workspace() {
  const principal = useSession()
  const {theme, setTheme, density, setDensity} = usePreferences()
  const [menuOpen, setMenuOpen] = useState(false)
  const [commandOpen, setCommandOpen] = useState(false)
  const [notifications, setNotifications] = useState(false)
  const [query, setQuery] = useState('')
  const location = useLocation()
  const navigate = useNavigate()
  const cache = useQueryClient()
  const jobs = useQuery({queryKey: ['jobs'], queryFn: ({signal}) => api<Job[]>('/api/jobs?limit=20', {signal}), refetchInterval: notifications ? 4000 : 30000})
  const info = useQuery({queryKey: ['system-info'], queryFn: () => api<{version: string}>('/api/system/info'), staleTime: Infinity})
  const running = (jobs.data ?? []).filter(job => ['queued', 'running', 'pending'].includes(job.status)).length
  useEffect(() => {document.documentElement.dataset.theme = theme; document.documentElement.dataset.density = density}, [theme, density])
  useEffect(() => {setMenuOpen(false)}, [location.pathname])
  useEffect(() => {
    const keydown = (event: KeyboardEvent) => {
      if ((event.ctrlKey || event.metaKey) && event.key.toLowerCase() === 'k') {event.preventDefault(); setCommandOpen(value => !value); return}
      const target = event.target as HTMLElement
      if (target.isContentEditable || ['INPUT', 'TEXTAREA', 'SELECT'].includes(target.tagName) || event.ctrlKey || event.metaKey || event.altKey) return
      if (event.key === 'c') navigate('/compose')
      if (event.key === '/') {event.preventDefault(); setCommandOpen(true)}
    }
    window.addEventListener('keydown', keydown)
    return () => window.removeEventListener('keydown', keydown)
  }, [navigate])
  function go(to: string) {navigate(to); setCommandOpen(false); setQuery('')}
  async function logout() {
    if (!window.dispatchEvent(new Event('postra:before-logout', {cancelable: true}))) return
    try {await api('/api/auth/logout', {method: 'POST'}); markSignedOut(); await cache.cancelQueries(); cache.clear(); notifyAuthChange('signed_out'); window.location.assign('/app/?sso=signed_out')}
    catch (error) {window.dispatchEvent(new Event('postra:logout-failed')); toast.error(error instanceof Error ? error.message : '로그아웃에 실패했습니다.')}
  }
  return <div className="workspace">
    <a className="skip-link" href="#workspace-main">본문으로 건너뛰기</a>
    {menuOpen && <button className="sidebar-overlay" aria-label="메뉴 닫기" onClick={() => setMenuOpen(false)}/>}
    <aside className={`sidebar ${menuOpen ? 'sidebar-open' : ''}`} aria-label="주 메뉴">
      <Link className="brand" to="/mail"><span className="brand-mark"><Mail size={21}/></span><div>Postra<span>AI WORK MAIL</span></div></Link>
      <Button className="compose-button" asChild><Link to="/compose"><Plus size={17}/>새 메일 작성 <kbd>C</kbd></Link></Button>
      <nav className="sidebar-nav">{navigation.map(group => <div className="nav-group" key={group.label}><div className="nav-label">{group.label}</div>{group.items.map(item => {
        const selected = item.to.includes('?') ? location.pathname === '/mail' && location.search.includes('folder=important') : location.pathname === item.to && !(item.to === '/mail' && location.search.includes('folder=important'))
        return <NavLink key={item.to} to={item.to} className={`nav-item ${selected ? 'selected' : ''}`}><item.icon size={17}/>{item.label}</NavLink>
      })}</div>)}{principal?.role === 'admin' && <div className="nav-group"><div className="nav-label">ADMIN</div><NavLink to="/admin" className={({isActive}) => `nav-item ${isActive ? 'selected' : ''}`}><ShieldCheck size={17}/>관리자 콘솔</NavLink></div>}</nav>
      <div className="sidebar-footer"><div className="identity"><span className="avatar">{(principal?.display_name || principal?.login_id || 'P').slice(0, 1)}</span><div><strong>{principal?.display_name || principal?.login_id}</strong><span>{principal?.role === 'admin' ? '관리자' : '내 워크스페이스'}</span></div><Button variant="ghost" size="icon" title="로그아웃" aria-label="로그아웃" onClick={logout}><LogOut size={16}/></Button></div><div className="legacy-link"><span>{info.data?.version || 'Postra'}</span><a href="/ui/">기존 UI</a></div></div>
    </aside>
    <div className="workspace-body">
      <header className="workspace-toolbar">
        <Button className="mobile-menu" variant="ghost" size="icon" aria-label="메뉴 열기" onClick={() => setMenuOpen(true)}><Menu size={20}/></Button>
        <button className="global-search" onClick={() => setCommandOpen(true)}><Search size={17}/><span>메일 검색, 업무 찾기, AI에게 질문…</span><kbd>⌘ K</kbd></button>
        <div className="toolbar-actions"><Button variant="ghost" size="icon" title={density === 'compact' ? '편안한 밀도' : '간결한 밀도'} aria-label="목록 밀도 변경" onClick={() => setDensity(density === 'compact' ? 'comfortable' : 'compact')}><LayoutList size={18}/></Button><Button variant="ghost" size="icon" aria-label={theme === 'dark' ? '밝은 테마' : '어두운 테마'} onClick={() => setTheme(theme === 'dark' ? 'light' : 'dark')}>{theme === 'dark' ? <Sun size={18}/> : <Moon size={18}/>}</Button><Button variant="ghost" size="icon" className="notification-trigger" aria-label={`작업 알림${running ? `, 진행 중 ${running}개` : ''}`} onClick={() => setNotifications(true)}><Bell size={18}/>{running > 0 && <span className="notification-dot"/>}</Button></div>
      </header>
      <main id="workspace-main" className="workspace-main"><Outlet/></main>
    </div>
    <Dialog.Root open={commandOpen} onOpenChange={setCommandOpen}><Dialog.Portal><Dialog.Overlay className="dialog-overlay"/><Dialog.Content className="command-dialog"><Dialog.Title className="sr-only">통합 검색 및 명령</Dialog.Title><Dialog.Description className="sr-only">메일을 검색하거나 원하는 화면으로 이동하세요.</Dialog.Description><Command><div className="command-input"><Search size={19}/><Command.Input placeholder="메일, 업무, 기능 검색…" value={query} onValueChange={setQuery}/><Dialog.Close asChild><Button variant="ghost" size="icon" aria-label="검색 닫기"><X size={16}/></Button></Dialog.Close></div><Command.List><Command.Empty>일치하는 기능이 없습니다.</Command.Empty>{query && <Command.Group heading="검색" forceMount>{[['keyword', '메일 키워드 검색'], ['semantic', '의미 기반 검색'], ['hybrid', '통합 검색']].map(([mode, label]) => <Command.Item key={mode} value={`${label} ${query}`} onSelect={() => go(`/mail?q=${encodeURIComponent(query)}&mode=${mode}`)}><Search size={16}/>{label}: {query}</Command.Item>)}<Command.Item value={`AI 질문 ${query}`} onSelect={() => go(`/ask?q=${encodeURIComponent(query)}`)}><Sparkles size={16}/>AI에게 질문: {query}</Command.Item></Command.Group>}<Command.Group heading="바로 가기">{navigation.flatMap(group => group.items).map(item => <Command.Item key={item.to} value={item.label} onSelect={() => go(item.to)}><item.icon size={16}/>{item.label}</Command.Item>)}<Command.Item value="새 메일 작성" onSelect={() => go('/compose')}><Plus size={16}/>새 메일 작성</Command.Item></Command.Group></Command.List><div className="command-footer"><CommandIcon size={13}/>↑↓ 이동 · Enter 선택 · Esc 닫기</div></Command></Dialog.Content></Dialog.Portal></Dialog.Root>
    <Dialog.Root open={notifications} onOpenChange={setNotifications}><Dialog.Portal><Dialog.Overlay className="dialog-overlay"/><Dialog.Content className="notifications-dialog"><div className="row between"><Dialog.Title>작업 알림</Dialog.Title><Dialog.Close asChild><Button variant="ghost" size="icon" aria-label="알림 닫기"><X size={18}/></Button></Dialog.Close></div><Dialog.Description className="muted">내 메일 동기화와 AI 작업의 진행 상태입니다.</Dialog.Description>{jobs.isPending ? <Loading/> : jobs.error ? <ErrorState error={jobs.error} retry={() => jobs.refetch()}/> : !(jobs.data?.length) ? <p className="muted">최근 작업이 없습니다.</p> : <ul className="job-list">{jobs.data.map(job => <li key={job.id}><div className="row between"><strong>{job.type}</strong><span className="badge">{job.status}</span></div><small className="muted">{formatDate(job.created_at)}</small>{(job.error || job.error_message) && <p role="alert">{job.error || job.error_message}</p>}</li>)}</ul>}</Dialog.Content></Dialog.Portal></Dialog.Root>
  </div>
}
