import {useEffect, useState} from 'react'
import {Link, useSearchParams} from 'react-router-dom'
import {useQuery} from '@tanstack/react-query'
import {Activity, Search} from 'lucide-react'
import {api} from '@/api/client'
import {Button, ErrorState, Input, PageHeader} from '@/components/ui'
import {SettingsEditor} from '@/features/settings/SettingsEditor'
import {ConnectionDiagnostic, useConnectionProbe} from '@/features/settings/ConnectionDiagnostic'
import type {SettingsView} from '@/features/settings/preferences'
import {UsersPanel, ProvisioningPanel, PurgePanel} from './users'
import {AuditPanel, IncidentsPanel} from './activity'
import {MCPAdminPage} from '@/features/mcp'

const categories = [['general', '일반'], ['auth', '인증 및 SSO'], ['mail', '메일'], ['ai', 'AI'], ['search', '검색 및 임베딩'], ['mcp', 'MCP'], ['send', '발송 정책'], ['attachments', '첨부파일'], ['sync', '동기화'], ['security', '보안'], ['notifications', '알림'], ['storage', '저장소'], ['system', '시스템'], ['users', '사용자 관리'], ['audit', '감사 로그']]
const aliases: Record<string, string> = {sso: 'auth', settings: 'sync', keys: 'mcp', data: 'storage', incidents: 'system'}
const states: Record<string, string> = {healthy: '정상', configured: '설정됨 · 연결 미확인', enabled: '활성', disabled: '사용 중지', unconfigured: '미설정', unavailable: '연결 확인 필요'}

function ConnectionTests({category}: {category: string}) {
  const configuration = useQuery({queryKey: ['configuration'], queryFn: ({signal}) => api<SettingsView>('/api/admin/configuration', {signal}), enabled: ['ai', 'search'].includes(category)})
  const probe = useConnectionProbe(`${category}:${configuration.data?.revision || ''}`)
  const tests = category === 'ai' ? [['/api/admin/ai/test', '현재 AI 연결 테스트']] : category === 'search' ? [['/api/admin/vector/test', '현재 임베딩·벡터 테스트']] : []
  return <>{tests.length > 0 && <div className="operations-tests">{tests.map(([path, label]) => <Button key={path} variant="outline" disabled={probe.busy || configuration.isPending} onClick={() => void probe.run(path)}><Activity size={15}/>{probe.busy ? '확인 중…' : label}</Button>)}<small className="muted">현재 저장된 값으로 확인합니다. 변경 후 다시 시험하세요.</small></div>}{probe.result && <ConnectionDiagnostic result={probe.result.data}/>} {probe.error != null && <ErrorState error={probe.error}/>}</>
}

export function OperationsConsole() {
  const [params, setParams] = useSearchParams()
  const requested = params.get('category') || aliases[params.get('tab') || ''] || params.get('tab') || 'general'
  const category = categories.some(([key]) => key === requested) ? requested : 'general'
  const [query, setQuery] = useState('')
  const [settingsPending, setSettingsPending] = useState(false)
  // Clear filters only after navigation is accepted. Clearing them before the
  // router guard runs can hide the editor while the user chooses to stay.
  useEffect(() => {setQuery('')}, [category])
  const showSettings = !!query || settingsPending || !['users', 'audit'].includes(category)
  const operations = useQuery({queryKey: ['operations'], queryFn: () => api<Record<string, string>>('/api/admin/operations'), refetchInterval: 30000})
  return <div className="page stack"><PageHeader title="운영 콘솔" description="조직 정책과 실제 적용 상태를 한 곳에서 관리합니다. 환경변수는 최초 기본값이며, 저장된 관리자 설정이 우선합니다."/>
    <label className="settings-search"><Search size={18}/><Input aria-label="관리자 설정 검색" placeholder="설정 이름, 기능 또는 POSTRA_AI_BASE_URL 검색" value={query} onChange={event => setQuery(event.target.value)}/></label>
    <section className="operations-status" aria-label="운영 상태">{[['ai', 'AI'], ['mail', 'Mail'], ['mcp', 'MCP'], ['database', 'Database'], ['oidc', 'OIDC']].map(([key, label]) => <div key={key}><strong>{label}</strong><span className="muted">{operations.isPending ? '확인 중' : operations.error ? '확인 불가' : states[operations.data?.[key] || ''] || '미확인'}</span></div>)}</section>
    <div className="operations-layout"><nav className="operations-nav" aria-label="관리자 설정 카테고리">{categories.map(([key, label]) => <Button key={key} variant={category === key && !query ? 'secondary' : 'ghost'} aria-current={category === key && !query ? 'page' : undefined} onClick={() => {if (key === category) setQuery(''); else setParams({category: key})}}>{label}</Button>)}</nav>
      <div className="operations-content stack">{!query && <h2>{categories.find(([key]) => key === category)?.[1]}</h2>}
        <div hidden={!showSettings}><ConnectionTests key={category} category={category}/><SettingsEditor admin category={category} query={query} onPendingChange={setSettingsPending}/></div>
        {!query && <>{category === 'users' && <UsersPanel/>}{category === 'auth' && <ProvisioningPanel/>}{category === 'mcp' && <MCPAdminPage/>}{category === 'audit' && <AuditPanel/>}{category === 'storage' && <PurgePanel/>}{category === 'system' && <IncidentsPanel/>}{category === 'security' && <Button asChild variant="outline"><Link to="/admin/tracking">방문 추적 · CSP 허용 및 위반 관리</Link></Button>}</>}
      </div>
    </div>
  </div>
}
