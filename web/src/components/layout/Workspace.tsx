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
import {usePreferenceBridge} from '@/features/settings/usePreferenceBridge'
import {NotificationEvents, NotificationsPanel, useNotificationSummary} from '@/features/notifications'
import {formatDate} from '@/lib/utils'
import {markSignedOut, notifyAuthChange} from '@/lib/auth-events'
import {dispatchMailCommand, mailCommandLabels, useAvailableMailCommands} from '@/lib/mail-commands'
import {createWorkspaceShortcuts} from '@/lib/workspace-shortcuts'
import './keyboard.css'
import {PageErrorBoundary} from './PageErrorBoundary'

const navigation = [
  {label: 'MAIL', items: [{to: '/mail', label: '받은메일', icon: Inbox}, {to: '/mail?folder=important', label: '중요 메일', icon: Star}, {to: '/sent', label: '발송 메일', icon: Mail}, {to: '/drafts', label: '초안', icon: FilePenLine}]},
  {label: 'WORK', items: [{to: '/work', label: '내 업무', icon: BriefcaseBusiness}, {to: '/team', label: '팀 업무함', icon: Users}, {to: '/actions', label: '액션 센터', icon: CheckSquare}]},
  {label: 'AI', items: [{to: '/ask', label: 'Ask Postra', icon: Sparkles}, {to: '/digest', label: '브리핑', icon: LayoutList}]},
  {label: 'WORKSPACE', items: [{to: '/accounts', label: '메일 계정', icon: Mail}, {to: '/rules', label: '자동화 규칙', icon: Settings}, {to: '/keys', label: 'MCP 연결 키', icon: ShieldCheck}, {to: '/jobs', label: '작업 상태', icon: LayoutList}, {to: '/settings', label: '개인 설정', icon: Settings}]},
]
type Job = {id: string; type: string; status: string; created_at: number; error?: string; error_message?: string}

export function Workspace() {
  const principal = useSession()
  const {theme, density} = usePreferences()
  const preferences = usePreferenceBridge()
  const [menuOpen, setMenuOpen] = useState(false)
  const [commandOpen, setCommandOpen] = useState(false)
  const [notifications, setNotifications] = useState(false)
  const [shortcutsOpen, setShortcutsOpen] = useState(false)
  const [query, setQuery] = useState('')
  const location = useLocation()
  const navigate = useNavigate()
  const cache = useQueryClient()
  const {running} = useNotificationSummary()
  const mailCommands = useAvailableMailCommands()
  const info = useQuery({queryKey: ['system-info'], queryFn: () => api<{version: string}>('/api/system/info'), staleTime: Infinity})

  useEffect(() => {setMenuOpen(false)}, [location.pathname])
  useEffect(() => {
    const shortcuts = createWorkspaceShortcuts({navigate, openCommand: () => setCommandOpen(true), showHelp: () => setShortcutsOpen(true), mail: dispatchMailCommand, closePanels: () => {if (!menuOpen) return false; setMenuOpen(false); return true}})
    window.addEventListener('keydown', shortcuts.keydown)
    window.addEventListener('compositionstart', shortcuts.compositionStart)
    window.addEventListener('compositionend', shortcuts.compositionEnd)
    window.addEventListener('blur', shortcuts.reset)
    return () => {window.removeEventListener('keydown', shortcuts.keydown); window.removeEventListener('compositionstart', shortcuts.compositionStart); window.removeEventListener('compositionend', shortcuts.compositionEnd); window.removeEventListener('blur', shortcuts.reset)}
  }, [navigate, menuOpen, location.pathname, location.search])
  function go(to: string) {navigate(to); setCommandOpen(false); setQuery('')}
  async function logout() {
    if (!window.dispatchEvent(new Event('postra:before-logout', {cancelable: true}))) return
    try {await api('/auth/logout', {method: 'POST'}); markSignedOut(); await cache.cancelQueries(); cache.clear(); notifyAuthChange('signed_out'); window.location.assign('/app/?sso=signed_out')}
    catch (error) {window.dispatchEvent(new Event('postra:logout-failed')); toast.error(error instanceof Error ? error.message : '로그아웃에 실패했습니다.')}
  }
  return <div className="workspace">
    <NotificationEvents/>
    <a className="skip-link" href="#workspace-main">본문으로 건너뛰기</a>
    {menuOpen && <button className="sidebar-overlay" aria-label="메뉴 닫기" onClick={() => setMenuOpen(false)}/>}
    <aside className={`sidebar ${menuOpen ? 'sidebar-open' : ''}`} aria-label="주 메뉴">
      <Link className="brand" to="/mail"><span className="brand-mark"><Mail size={21}/></span><div>{preferences.data?.runtime?.['general.product_name'] || 'Postra'}<span>AI WORK MAIL</span></div></Link>
      <Button className="compose-button" asChild><Link to="/compose"><Plus size={17}/>새 메일 작성 <kbd>C</kbd></Link></Button>
      <nav className="sidebar-nav">{navigation.map(group => <div className="nav-group" key={group.label}><div className="nav-label">{group.label}</div>{group.items.map(item => {
        const selected = item.to.includes('?') ? location.pathname === '/mail' && location.search.includes('folder=important') : location.pathname === item.to && !(item.to === '/mail' && location.search.includes('folder=important'))
        return <NavLink key={item.to} to={item.to} className={`nav-item ${selected ? 'selected' : ''}`}><item.icon size={17}/>{item.label}</NavLink>
      })}</div>)}{principal?.role === 'admin' && <div className="nav-group"><div className="nav-label">ADMIN</div><NavLink to="/admin" className={({isActive}) => `nav-item ${isActive ? 'selected' : ''}`}><ShieldCheck size={17}/>관리자 콘솔</NavLink></div>}</nav>
      <div className="sidebar-footer"><div className="identity"><span className="avatar">{(principal?.display_name || principal?.login_id || 'P').slice(0, 1)}</span><div><strong>{principal?.display_name || principal?.login_id}</strong><span>{principal?.role === 'admin' ? '관리자' : '내 워크스페이스'}</span></div><Button variant="ghost" size="icon" title="로그아웃" aria-label="로그아웃" onClick={logout}><LogOut size={16}/></Button></div><div className="legacy-link"><span>{info.data?.version || 'Postra'}</span><Link to="/settings">개인 설정</Link></div></div>
    </aside>
    <div className="workspace-body">
      <header className="workspace-toolbar">
        <Button className="mobile-menu" variant="ghost" size="icon" aria-label="메뉴 열기" onClick={() => setMenuOpen(true)}><Menu size={20}/></Button>
        <button className="global-search" onClick={() => setCommandOpen(true)}><Search size={17}/><span>메일 검색, 업무 찾기, AI에게 질문…</span><kbd>⌘ K</kbd></button>
        <div className="toolbar-actions"><Button variant="ghost" size="icon" title={density === 'compact' ? '편안한 밀도' : '간결한 밀도'} aria-label="목록 밀도 변경" disabled={!preferences.data || preferences.locked('ui.density')} onClick={() => preferences.update('ui.density', density === 'compact' ? 'comfortable' : 'compact')}><LayoutList size={18}/></Button><Button variant="ghost" size="icon" aria-label={theme === 'dark' ? '밝은 테마' : '어두운 테마'} disabled={!preferences.data || preferences.locked('ui.theme')} onClick={() => preferences.update('ui.theme', theme === 'dark' ? 'light' : 'dark')}>{theme === 'dark' ? <Sun size={18}/> : <Moon size={18}/>}</Button><Button variant="ghost" size="icon" className="notification-trigger" aria-label={`작업 알림${running ? `, 진행 중 ${running}개` : ''}`} onClick={() => setNotifications(true)}><Bell size={18}/>{running > 0 && <span className="notification-dot"/>}</Button></div>
      </header>
      <main id="workspace-main" className="workspace-main"><PageErrorBoundary resetKey={location.key}><Outlet/></PageErrorBoundary></main>
    </div>
    <Dialog.Root open={commandOpen} onOpenChange={setCommandOpen}><Dialog.Portal><Dialog.Overlay className="dialog-overlay"/><Dialog.Content className="command-dialog"><Dialog.Title className="sr-only">통합 검색 및 명령</Dialog.Title><Dialog.Description className="sr-only">메일을 검색하거나 원하는 화면으로 이동하세요.</Dialog.Description><Command><div className="command-input"><Search size={19}/><Command.Input placeholder="메일, 업무, 기능 검색…" value={query} onValueChange={setQuery}/><Dialog.Close asChild><Button variant="ghost" size="icon" aria-label="검색 닫기"><X size={16}/></Button></Dialog.Close></div><Command.List><Command.Empty>일치하는 기능이 없습니다.</Command.Empty>{query && <Command.Group heading="검색" forceMount>{[['keyword', '메일 키워드 검색'], ['semantic', '의미 기반 검색'], ['hybrid', '통합 검색']].map(([mode, label]) => <Command.Item key={mode} value={`${label} ${query}`} onSelect={() => go(`/mail?q=${encodeURIComponent(query)}&mode=${mode}`)}><Search size={16}/>{label}: {query}</Command.Item>)}<Command.Item value={`AI 질문 ${query}`} onSelect={() => go(`/ask?q=${encodeURIComponent(query)}`)}><Sparkles size={16}/>AI에게 질문: {query}</Command.Item></Command.Group>}{mailCommands.length > 0 && <Command.Group heading="현재 메일 · 조회한 범위만">{mailCommands.map(command => <Command.Item key={command} value={mailCommandLabels[command].label} onSelect={() => {setCommandOpen(false); setQuery(''); dispatchMailCommand(command)}}><Mail size={16}/>{mailCommandLabels[command].label}{mailCommandLabels[command].key && <kbd>{mailCommandLabels[command].key}</kbd>}</Command.Item>)}</Command.Group>}<Command.Group heading="바로 가기">{navigation.flatMap(group => group.items).map(item => <Command.Item key={item.to} value={item.label} onSelect={() => go(item.to)}><item.icon size={16}/>{item.label}</Command.Item>)}{principal?.role === 'admin' && <Command.Item value="관리자 콘솔" onSelect={() => go('/admin')}><ShieldCheck size={16}/>관리자 콘솔</Command.Item>}<Command.Item value="새 메일 작성" onSelect={() => go('/compose')}><Plus size={16}/>새 메일 작성</Command.Item><Command.Item value="키보드 단축키 도움말" onSelect={() => {setCommandOpen(false); setShortcutsOpen(true)}}><CommandIcon size={16}/>키보드 단축키 도움말 <kbd>?</kbd></Command.Item></Command.Group></Command.List><div className="command-footer"><CommandIcon size={13}/>↑↓ 이동 · Enter 선택 · Esc 닫기</div></Command></Dialog.Content></Dialog.Portal></Dialog.Root>
    <Dialog.Root open={shortcutsOpen} onOpenChange={setShortcutsOpen}><Dialog.Portal><Dialog.Overlay className="dialog-overlay"/><Dialog.Content className="notifications-dialog"><div className="row between"><Dialog.Title>키보드 단축키</Dialog.Title><Dialog.Close asChild><Button variant="ghost" size="icon" aria-label="단축키 도움말 닫기"><X size={18}/></Button></Dialog.Close></div><Dialog.Description className="muted">입력·편집·한국어 조합 중에는 실행하지 않습니다. 메일 명령은 현재 열거나 불러온 내 메일에만 적용합니다. 본문 프레임에 포커스가 있으면 먼저 메일 도구 모음으로 이동하세요.</Dialog.Description><dl className="shortcut-help">{[['C','새 메일 작성'],['⌘ / Ctrl K 또는 /','통합 검색·명령'],['G 다음 I','받은메일로 이동'],['G 다음 W','내 업무로 이동'],...Object.values(mailCommandLabels).filter(item=>item.key).map(item=>[item.key,item.label]),['?','이 도움말']].map(([key,label])=><div className="row between" key={key}><dt><kbd>{key}</kbd></dt><dd>{label}</dd></div>)}</dl><p className="muted small">답장·전달은 초안만 만듭니다. 단축키로 승인하거나 발송하지 않습니다. J/K는 불러온 검색 결과를 벗어나지 않습니다.</p></Dialog.Content></Dialog.Portal></Dialog.Root>
    <Dialog.Root open={notifications} onOpenChange={setNotifications}><Dialog.Portal><Dialog.Overlay className="dialog-overlay"/><Dialog.Content className="notifications-dialog"><div className="row between"><Dialog.Title>작업 알림</Dialog.Title><Dialog.Close asChild><Button variant="ghost" size="icon" aria-label="알림 닫기"><X size={18}/></Button></Dialog.Close></div><Dialog.Description className="muted">내 메일 동기화와 AI 작업의 진행 상태입니다.</Dialog.Description><NotificationsPanel/></Dialog.Content></Dialog.Portal></Dialog.Root>
  </div>
}
