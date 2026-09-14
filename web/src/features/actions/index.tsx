import {useState} from 'react'
import {Link} from 'react-router-dom'
import {useMutation, useQuery, useQueryClient} from '@tanstack/react-query'
import {CalendarDays, Check, CheckSquare, Download, UserRound} from 'lucide-react'
import {toast} from 'sonner'
import {api} from '@/api/client'
import {Badge, Button, EmptyState, ErrorState, Loading, PageHeader} from '@/components/ui'

type ActionCard = {id: string; message_id: string; type: string; title: string; detail?: string; due?: string; assignee?: string; status: string; confidence?: number}
const states: Record<string, string> = {pending: '검토 대기', approved: '승인됨', done: '완료', rejected: '제외됨', exported: '내보냄'}
export function ActionsPage() {
  const [status, setStatus] = useState('')
  const cache = useQueryClient()
  const cards = useQuery({queryKey: ['action-cards', status], queryFn: ({signal}) => api<{cards: ActionCard[]}>(`/api/action-cards?limit=200&status=${status}`, {signal})})
  const update = useMutation({mutationFn: ({id, status}: {id: string; status: string}) => api(`/api/action-cards/${id}/status`, {body: {status}}), onSuccess: () => {void cache.invalidateQueries({queryKey: ['action-cards']}); toast.success('액션 상태를 저장했습니다.')}, onError: error => toast.error(error.message)})
  const exportCard = useMutation({mutationFn: (id: string) => api(`/api/action-cards/${id}/export`, {body: {target: 'calendar'}}), onSuccess: result => {
    const url = URL.createObjectURL(new Blob([JSON.stringify(result, null, 2)], {type: 'application/json'}))
    const anchor = document.createElement('a'); anchor.href = url; anchor.download = 'postra-action.json'; anchor.click(); URL.revokeObjectURL(url)
    void cache.invalidateQueries({queryKey: ['action-cards']}); toast.success('연동용 JSON을 저장했습니다. 외부 서비스에 자동 등록되지는 않습니다.')
  }, onError: error => toast.error(error.message)})
  return <div className="page"><PageHeader title="액션 센터" description="메일에서 추출한 할 일과 일정을 검토하고, 실행 가능한 업무로 연결하세요."/><div className="tabs"><button className={!status ? 'active' : ''} onClick={() => setStatus('')}>전체</button>{Object.entries(states).map(([id, label]) => <button key={id} className={status === id ? 'active' : ''} onClick={() => setStatus(id)}>{label}</button>)}</div>{cards.isPending ? <Loading/> : cards.error ? <ErrorState error={cards.error} retry={() => cards.refetch()}/> : !(cards.data?.cards?.length) ? <EmptyState title="아직 추출된 액션이 없습니다" description="메일의 AI Insight에서 액션 추출을 실행해 보세요."/> : <div className="action-grid">{cards.data.cards.map(card => <article className="action-item" key={card.id}><div className="row between"><Badge variant="secondary">{card.type === 'meeting' ? '일정' : card.type === 'approval' ? '승인' : '할 일'}</Badge><Badge variant={card.status === 'done' ? 'default' : 'outline'}>{states[card.status] || card.status}</Badge></div><h2>{card.title}</h2>{card.detail && <p className="muted pre-wrap">{card.detail}</p>}<div className="stack small">{card.due && <span className="row"><CalendarDays size={14}/>{card.due}</span>}{card.assignee && <span className="row"><UserRound size={14}/>{card.assignee}</span>}<Link to={`/mail?message=${card.message_id}`}>원본 메일 보기 →</Link></div><div className="row action-controls">{card.status === 'pending' && <><Button size="sm" onClick={() => update.mutate({id: card.id, status: 'approved'})} disabled={update.isPending}><Check size={14}/>승인</Button><Button variant="ghost" size="sm" onClick={() => update.mutate({id: card.id, status: 'rejected'})} disabled={update.isPending}>제외</Button></>}{['pending', 'approved', 'exported'].includes(card.status) && <Button variant="outline" size="sm" onClick={() => update.mutate({id: card.id, status: 'done'})} disabled={update.isPending}><CheckSquare size={14}/>완료</Button>}{['approved', 'exported'].includes(card.status) && <Button variant="ghost" size="sm" disabled={exportCard.isPending} onClick={() => exportCard.mutate(card.id)}><Download size={14}/>JSON 내보내기</Button>}{['done', 'rejected'].includes(card.status) && <Button variant="ghost" size="sm" onClick={() => update.mutate({id: card.id, status: 'pending'})} disabled={update.isPending}>다시 검토</Button>}</div></article>)}</div>}<p className="small muted">AI가 추출한 날짜·담당자는 원문과 대조해 주세요. 내보내기는 연동용 데이터 생성이며 외부 시스템에 자동으로 작업을 만들지 않습니다.</p></div>
}
