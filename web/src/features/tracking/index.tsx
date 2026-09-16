import {useLocation} from 'react-router-dom'
import {useMutation, useQuery, useQueryClient} from '@tanstack/react-query'
import {z} from 'zod'
import {api} from '@/api/client'
import {nullableList, parseResponse} from '@/api/response'
import {Badge, Button, EmptyState, ErrorState, Loading, PageHeader, Panel} from '@/components/ui'
import {SettingsEditor} from '@/features/settings/SettingsEditor'

export function trackingPage(pathname: string): string {
  if (/^\/admin(?:\/|$)/.test(pathname)) return '/app/admin'
  if (/^\/(mail|search|work|team|actions|ask|digest|rules|sent|drafts|compose|accounts|jobs)$/.test(pathname)) return '/app'+pathname
  const match=pathname.match(/^\/(messages|threads|drafts|accounts|jobs)\/[^/]+$/)
  return match ? `/app/${match[1]}/:id` : ''
}

// The tracker never executes in React's document. An opaque frame has no
// access to mail DOM, passwords, session cookies, or local/session storage.
export function BrowserTracking() {
  const location=useLocation()
  const page=trackingPage(location.pathname)
  const config=useQuery({queryKey:['tracking',page],queryFn:({signal})=>api<{active:boolean;frame_url?:string}>(`/api/tracking?page=${encodeURIComponent(page)}`,{signal}),enabled:!!page,staleTime:30000,retry:false})
  if (!page || !config.data?.active || !config.data.frame_url?.startsWith('/api/tracking/frame?')) return null
  return <iframe title="개인정보 격리 방문 통계" aria-hidden="true" tabIndex={-1} hidden sandbox="allow-scripts" referrerPolicy="no-referrer" src={config.data.frame_url}/>
}

const trackingSchema=z.object({enabled:z.boolean(),provider:z.string(),isolated:z.boolean(),violations:nullableList(z.object({origin:z.string(),directive:z.string(),page:z.string(),count:z.number(),last_seen:z.string(),allowed:z.boolean()}))})
export function TrackingPage() {
  const cache=useQueryClient()
  const state=useQuery({queryKey:['admin-tracking'],queryFn:async()=>parseResponse(trackingSchema,await api<unknown>('/api/admin/tracking')),refetchInterval:15000})
  const change=useMutation({mutationFn:(origin?:string)=>api(origin?'/api/admin/tracking/allow':'/api/admin/tracking/violations',{method:origin?'POST':'DELETE',...(origin?{body:{origin}}:{})}),onSuccess:()=>{void cache.invalidateQueries({queryKey:['admin-tracking']});void cache.invalidateQueries({queryKey:['tracking']});void cache.invalidateQueries({queryKey:['settings']})}})
  return <div className="page stack"><PageHeader title="방문 통계 · CSP 관리" description="방문 통계는 기본 비활성화입니다. 활성화해도 격리 프레임에만 실행되며, 메일 본문·검색어·사용자 식별자는 전달하지 않습니다."/>
    <Panel><h2>개인정보 격리</h2><p className="muted">로그인·설정·MCP 키 화면에는 추적을 실행하지 않습니다. 관리자 페이지는 별도 포함 옵션을 켠 경우에만 집계합니다. SPA 경로는 식별자 없는 템플릿으로만 전달됩니다. 쿠키나 최상위 DOM 접근이 필요한 일부 맞춤 스크립트는 격리 정책상 동작하지 않을 수 있습니다.</p><p>오프라인망에서는 내부 Momento 수집기와 동일 출처 프록시를 사용할 수 있습니다. 프록시는 인증·세션·CSRF 정보를 전달하거나 수집기 쿠키를 저장하지 않습니다.</p></Panel>
    <SettingsEditor admin category="security" query="tracking."/>
    <Panel><div className="row between"><h2>차단된 외부 출처</h2><Button size="sm" variant="outline" disabled={change.isPending} onClick={()=>change.mutate(undefined)}>기록 비우기</Button></div><p className="small muted">최근 브라우저의 차단 기록이며 메모리에 최대 100개 보관합니다. 보고 내용은 신뢰할 수 없는 제안입니다. 실제 운영 수집기 주소인지 확인한 후에만 허용하세요.</p>{change.error&&<ErrorState error={change.error}/>}{state.isPending?<Loading/>:state.error?<ErrorState error={state.error} retry={()=>state.refetch()}/>:!state.data?.violations.length?<EmptyState title="차단 기록이 없습니다"/>:state.data.violations.map(item=><article className="outbound-item" key={item.directive+' '+item.origin}><div><h3>{item.origin}</h3><p className="small muted">{item.directive} · {item.page} · {item.count}회</p></div>{item.allowed?<Badge variant="secondary">허용됨</Badge>:<Button size="sm" variant="outline" disabled={change.isPending} onClick={()=>{if(window.confirm(`${item.origin} 출처를 추적 스크립트 정책에 추가하시겠습니까? 신뢰하는 운영 수집기인지 확인하세요.`))change.mutate(item.origin)}}>검토 후 허용</Button>}</article>)}</Panel>
  </div>
}
