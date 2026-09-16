import {useState, type FormEvent} from 'react'
import {Link, useSearchParams} from 'react-router-dom'
import {useMutation, useQuery, useQueryClient} from '@tanstack/react-query'
import {CalendarDays, CheckSquare, Download, Plus, UserRound} from 'lucide-react'
import {toast} from 'sonner'
import {api} from '@/api/client'
import {Badge, Button, EmptyState, ErrorState, Input, Loading, PageHeader, Panel, Textarea} from '@/components/ui'
import {actionGroup, actionGroups, type ActionGroup} from './grouping'
import {parseActionCards} from './response'

const states: Record<string, string> = {pending: '검토 대기', approved: '승인됨', done: '완료', rejected: '제외됨', exported: '내보냄'}
export function ActionsPage() {
  const [params] = useSearchParams()
  const [creating, setCreating] = useState(params.get('create') === '1')
  const [status, setStatus] = useState('')
  const [group, setGroup] = useState<ActionGroup | ''>('')
  const cache = useQueryClient()
  const cards = useQuery({queryKey: ['action-cards', status], queryFn: async ({signal}) => parseActionCards(await api(`/api/action-cards?limit=200&status=${status}`, {signal}))})
  const actionCards = cards.data?.cards ?? []
  const update = useMutation({mutationFn: ({id, status}: {id: string; status: string}) => api(`/api/action-cards/${encodeURIComponent(id)}/status`, {body: {status}}), onSuccess: () => {void cache.invalidateQueries({queryKey: ['action-cards']}); toast.success('액션 상태를 저장했습니다.')}, onError: error => toast.error(error.message)})
  const exportCard = useMutation({mutationFn: (id: string) => api(`/api/action-cards/${encodeURIComponent(id)}/export`, {body: {target: 'calendar'}}), onSuccess: result => {
    const url = URL.createObjectURL(new Blob([JSON.stringify(result, null, 2)], {type: 'application/json'}))
    const anchor = document.createElement('a'); anchor.href = url; anchor.download = 'postra-action.json'; anchor.click(); URL.revokeObjectURL(url)
    void cache.invalidateQueries({queryKey: ['action-cards']}); toast.success('연동용 JSON을 저장했습니다. 외부 서비스에 자동 등록되지는 않습니다.')
  }, onError: error => toast.error(error.message)})
  const now = new Date()
  return <div className="page"><PageHeader title="액션 센터" description="기한별 업무를 확인하고 원본 메일에 연결한 액션을 직접 만들거나 검토하세요." actions={<Button onClick={() => setCreating(value => !value)}><Plus size={16}/>액션 만들기</Button>}/>
    {creating && <CreateActionForm messageID={params.get('message') || ''} onClose={() => setCreating(false)}/>}
    <label className="field">승인·완료 상태<select className="input" value={status} onChange={event => setStatus(event.target.value)}><option value="">모든 상태</option>{Object.entries(states).map(([id, label]) => <option key={id} value={id}>{label}</option>)}</select></label>
    <div className="tabs" aria-label="액션 기한 분류"><button className={!group ? 'active' : ''} onClick={() => setGroup('')}>전체</button>{actionGroups.map(item => <button key={item.id} className={group === item.id ? 'active' : ''} onClick={() => setGroup(item.id)}>{item.label} ({actionCards.filter(card => actionGroup(card, now) === item.id).length})</button>)}</div>
    {cards.isPending ? <Loading/> : cards.error ? <ErrorState error={cards.error} retry={() => cards.refetch()}/> : !actionCards.length ? <EmptyState title="아직 등록된 액션이 없습니다" description="직접 액션을 만들거나 메일의 AI Insight에서 추출하세요."/> : actionGroups.filter(item => !group || group === item.id).map(item => {
      const selected = actionCards.filter(card => actionGroup(card, now) === item.id)
      return <section className="stack" aria-label={item.label} key={item.id}><h2>{item.label}</h2>{!selected.length ? <p className="muted">해당 액션이 없습니다.</p> : <div className="action-grid">{selected.map(card => <article className="action-item" key={card.id}>
        <div className="row between"><Badge variant="secondary">{card.type === 'meeting' ? '일정' : card.type === 'approval' ? '승인' : '할 일'}</Badge><Badge>{states[card.status] || '상태 확인 필요'}</Badge></div><h2>{card.title}</h2>{card.detail && <p className="muted pre-wrap">{card.detail}</p>}
        <div className="stack small">{card.due && <span className="row"><CalendarDays size={14}/>{card.due}</span>}{card.assignee && <span className="row"><UserRound size={14}/>{card.assignee}</span>}<Link to={`/mail?message=${encodeURIComponent(card.message_id)}`}>원본 메일 보기 →</Link><Link to={`/team?message=${encodeURIComponent(card.message_id)}`}>SLA·담당자·메모 관리 →</Link></div>
        <div className="row action-controls">{card.status === 'pending' && <><Button size="sm" onClick={() => update.mutate({id: card.id, status: 'approved'})} disabled={update.isPending}>승인</Button><Button variant="ghost" size="sm" onClick={() => update.mutate({id: card.id, status: 'rejected'})} disabled={update.isPending}>제외</Button></>}{['pending', 'approved', 'exported'].includes(card.status) && <Button variant="outline" size="sm" onClick={() => update.mutate({id: card.id, status: 'done'})} disabled={update.isPending}><CheckSquare size={14}/>완료</Button>}{['approved', 'exported'].includes(card.status) && <Button variant="ghost" size="sm" disabled={exportCard.isPending} onClick={() => exportCard.mutate(card.id)}><Download size={14}/>JSON 내보내기</Button>}{['done', 'rejected'].includes(card.status) && <Button variant="ghost" size="sm" onClick={() => update.mutate({id: card.id, status: 'pending'})} disabled={update.isPending}>다시 검토</Button>}</div>
      </article>)}</div>}</section>
    })}<p className="small muted">날짜가 없거나 자연어로 적힌 AI 기한은 대기·기한 확인으로 분류합니다. 날짜는 브라우저 현지 기준입니다. JSON 내보내기는 외부 등록이나 업무 완료가 아닙니다. 최근 최대 200건을 표시합니다.</p>
  </div>
}

function CreateActionForm({messageID: initialID, onClose}: {messageID: string; onClose: () => void}) {
  const [messageID, setMessageID] = useState(initialID)
  const [title, setTitle] = useState('')
  const [detail, setDetail] = useState('')
  const [due, setDue] = useState('')
  const [assignee, setAssignee] = useState('')
  const [type, setType] = useState('todo')
  const cache = useQueryClient()
  const create = useMutation({mutationFn: () => api('/api/action-cards', {method: 'POST', body: {message_id: messageID.trim(), title: title.trim(), detail, due, assignee: assignee.trim(), type}}), onSuccess: () => {void cache.invalidateQueries({queryKey: ['action-cards']}); toast.success('검토 대기 액션을 만들었습니다.'); onClose()}})
  function submit(event: FormEvent) {event.preventDefault(); create.mutate()}
  return <Panel aria-label="수동 액션 만들기"><form className="stack" onSubmit={submit}><h2>메일에 액션 연결</h2><p className="muted">직접 입력한 내용만 저장하며 AI나 외부 서비스는 호출하지 않습니다.</p>
    <label className="field">원본 메일 ID<Input required value={messageID} onChange={event => setMessageID(event.target.value)} placeholder="내 메일의 ID"/></label><label className="field">액션 제목<Input required maxLength={300} value={title} onChange={event => setTitle(event.target.value)}/></label>
    <label className="field">유형<select className="input" value={type} onChange={event => setType(event.target.value)}><option value="todo">할 일</option><option value="meeting">일정</option><option value="approval">승인</option><option value="inquiry">문의</option><option value="other">기타</option></select></label>
    <label className="field">상세 내용<Textarea maxLength={10000} value={detail} onChange={event => setDetail(event.target.value)}/></label><div className="grid"><label className="field">액션 기한<Input type="date" value={due} onChange={event => setDue(event.target.value)}/></label><label className="field">액션 담당자<Input maxLength={128} value={assignee} onChange={event => setAssignee(event.target.value)}/></label></div>
    {Boolean(create.error) && <ErrorState error={create.error}/>}<div className="row"><Button type="submit" disabled={create.isPending || !messageID.trim() || !title.trim()}>액션 저장</Button><Button variant="ghost" disabled={create.isPending} onClick={onClose}>닫기</Button></div>
  </form></Panel>
}
