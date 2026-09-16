import {useEffect, useRef, useState} from 'react'
import {Link, useLocation, useSearchParams} from 'react-router-dom'
import {useMutation, useQuery, useQueryClient} from '@tanstack/react-query'
import {CalendarDays, CheckSquare, Download, Plus, RefreshCw, UserRound} from 'lucide-react'
import {toast} from 'sonner'
import {api} from '@/api/client'
import {Badge, Button, EmptyState, ErrorState, Input, Loading, PageHeader} from '@/components/ui'
import {actionGroup, actionGroups, type ActionGroup} from './grouping'
import {parseActionCards} from './response'
import {CreateActionForm} from './CreateActionForm'
import './actions.css'

const states: Record<string, string> = {pending: '검토 대기', approved: '승인됨', done: '완료', rejected: '제외됨', exported: '내보냄'}
const types: Record<string, string> = {meeting: '일정', approval: '승인', todo: '할 일', inquiry: '문의', other: '기타'}
function labelFor(labels: Record<string, string>, value: string, fallback: string) {return Object.hasOwn(labels, value) ? labels[value] : fallback}
export function ActionsPage() {
  const [params] = useSearchParams()
  const location = useLocation()
  const [creating, setCreating] = useState(params.get('create') === '1')
  // React Router can change only the query string without remounting this
  // page. Open new URL intents, but do not reset an already mounted editor.
  useEffect(() => {if (params.get('create') === '1') setCreating(true)}, [location.key, params])
  const mutationLock = useRef(false)
  const [status, setStatus] = useState('')
  const [group, setGroup] = useState<ActionGroup | ''>('')
  const [query, setQuery] = useState('')
  const [now, setNow] = useState(() => new Date())
  useEffect(() => {const timer = window.setInterval(() => setNow(new Date()), 60000); return () => window.clearInterval(timer)}, [])
  const cache = useQueryClient()
  const cards = useQuery({queryKey: ['action-cards', status], queryFn: async ({signal}) => parseActionCards(await api(`/api/action-cards?limit=200&status=${status}`, {signal}))})
  const actionCards = cards.data?.cards ?? []
  const matching = actionCards.filter(card => `${card.title}\n${card.detail || ''}\n${card.assignee || ''}`.toLocaleLowerCase().includes(query.trim().toLocaleLowerCase()))
  const visible = matching.filter(card => !group || actionGroup(card, now) === group)
  const filtered = !!(status || group || query.trim())
  function resetFilters() {setStatus(''); setGroup(''); setQuery('')}
  const update = useMutation({mutationFn: ({id, status}: {id: string; status: string}) => api(`/api/action-cards/${encodeURIComponent(id)}/status`, {body: {status}}), onSuccess: async () => {await cache.invalidateQueries({queryKey: ['action-cards']}); toast.success('액션 상태를 저장했습니다.')}, onError: error => toast.error(error.message), onSettled: () => {mutationLock.current = false}})
  const exportCard = useMutation({mutationFn: (id: string) => api(`/api/action-cards/${encodeURIComponent(id)}/export`, {body: {target: 'calendar'}}), onSuccess: async result => {
    const url = URL.createObjectURL(new Blob([JSON.stringify(result, null, 2)], {type: 'application/json'}))
    const anchor = document.createElement('a'); anchor.href = url; anchor.download = 'postra-action.json'; anchor.click(); URL.revokeObjectURL(url)
    await cache.invalidateQueries({queryKey: ['action-cards']}); toast.success('연동용 JSON을 저장했습니다. 외부 서비스에 자동 등록되지는 않습니다.')
  }, onError: error => toast.error(error.message), onSettled: () => {mutationLock.current = false}})
  const mutating = update.isPending || exportCard.isPending
  function changeStatus(id: string, status: string) {if (mutationLock.current) return; mutationLock.current = true; update.mutate({id, status})}
  function exportAction(id: string) {if (mutationLock.current) return; mutationLock.current = true; exportCard.mutate(id)}
  return <div className="page action-center"><PageHeader title="액션 센터" description="메일에서 할 일을 만들고, 기한과 담당자별로 찾아 처리하세요." actions={<><Button variant="outline" size="icon" aria-label="액션 새로고침" disabled={cards.isFetching} onClick={() => cards.refetch()}><RefreshCw size={16}/></Button><Button disabled={creating} onClick={() => setCreating(true)}><Plus size={16}/>액션 만들기</Button></>}/>
    {creating && <CreateActionForm key={params.get('message') || ''} messageID={params.get('message') || ''} onClose={showActions => {setCreating(false); if (showActions) resetFilters()}}/>}
    <div className="action-filters"><label className="field">액션 검색<Input type="search" placeholder="제목, 상세 내용, 담당자" value={query} onChange={event => setQuery(event.target.value)}/></label><label className="field">승인·완료 상태<select className="input" value={status} onChange={event => setStatus(event.target.value)}><option value="">모든 상태</option>{Object.entries(states).map(([id, label]) => <option key={id} value={id}>{label}</option>)}</select></label><Button variant="ghost" disabled={!filtered} onClick={resetFilters}>필터 초기화</Button></div>
    <div className="tabs action-group-tabs" aria-label="액션 기한 분류"><button type="button" aria-pressed={!group} className={!group ? 'active' : ''} onClick={() => setGroup('')}>전체</button>{actionGroups.map(item => <button type="button" aria-pressed={group === item.id} key={item.id} className={group === item.id ? 'active' : ''} onClick={() => setGroup(item.id)}>{item.label} ({matching.filter(card => actionGroup(card, now) === item.id).length})</button>)}</div>
    {!cards.isPending && !cards.error && <p className="muted action-result-count" role="status">{visible.length}개 표시 · 선택한 상태의 최근 최대 200건 안에서 검색합니다.</p>}
    {cards.isPending ? <Loading/> : cards.error ? <ErrorState error={cards.error} retry={() => cards.refetch()}/> : !visible.length ? <EmptyState title={filtered ? '조건에 맞는 액션이 없습니다' : '아직 등록된 액션이 없습니다'} description={filtered ? '검색어 또는 기한·상태 필터를 바꿔 보세요.' : '액션 만들기에서 메일을 선택하거나 메일의 AI Insight에서 추출하세요.'} action={filtered ? <Button variant="outline" onClick={resetFilters}>모든 액션 보기</Button> : undefined}/> : actionGroups.filter(item => !group || group === item.id).map(item => {
      const selected = matching.filter(card => actionGroup(card, now) === item.id)
      if (!selected.length) return null
      return <section className="stack action-group" aria-label={item.label} key={item.id}><h2>{item.label} · {selected.length}</h2><div className="action-grid">{selected.map(card => <article className="action-item" key={card.id} data-due={item.id}>
        <div className="row between"><Badge variant="secondary">{labelFor(types, card.type, '기타')}</Badge><Badge>{labelFor(states, card.status, '상태 확인 필요')}</Badge></div><h2>{card.title}</h2>{card.detail && <p className="muted pre-wrap">{card.detail}</p>}
        <div className="stack small">{card.due && <span className="row"><CalendarDays size={14}/>{card.due}</span>}{card.assignee && <span className="row"><UserRound size={14}/>{card.assignee}</span>}<Link to={`/mail?message=${encodeURIComponent(card.message_id)}`}>원본 메일 보기 →</Link><Link to={`/team?message=${encodeURIComponent(card.message_id)}`}>SLA·담당자·메모 관리 →</Link></div>
        <div className="row action-controls">{card.status === 'pending' && <><Button size="sm" onClick={() => changeStatus(card.id, 'approved')} disabled={mutating}>승인</Button><Button variant="ghost" size="sm" onClick={() => changeStatus(card.id, 'rejected')} disabled={mutating}>제외</Button></>}{['pending', 'approved', 'exported'].includes(card.status) && <Button variant="outline" size="sm" onClick={() => changeStatus(card.id, 'done')} disabled={mutating}><CheckSquare size={14}/>완료</Button>}{['approved', 'exported'].includes(card.status) && <Button variant="ghost" size="sm" disabled={mutating} onClick={() => exportAction(card.id)}><Download size={14}/>JSON 내보내기</Button>}{['done', 'rejected'].includes(card.status) && <Button variant="ghost" size="sm" onClick={() => changeStatus(card.id, 'pending')} disabled={mutating}>다시 검토</Button>}</div>
      </article>)}</div></section>
    })}<p className="small muted">날짜가 없거나 자연어로 적힌 AI 기한은 대기·기한 확인으로 분류합니다. 날짜는 브라우저 현지 기준입니다. JSON 내보내기는 외부 등록이나 업무 완료가 아닙니다. 최근 최대 200건을 표시합니다.</p>
  </div>
}
