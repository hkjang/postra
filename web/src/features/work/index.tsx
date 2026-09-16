import { useEffect, useId, useState, type FormEvent } from 'react';
import { Link, useSearchParams } from 'react-router-dom';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { Clock, MessageSquare, RefreshCw, UserRound } from 'lucide-react';
import { toast } from 'sonner';
import { api } from '@/api/client';
import { useSession } from '@/app/session';
import { Badge, Button, EmptyState, ErrorState, Input, Loading, PageHeader, Textarea } from '@/components/ui';
import { formatDate } from '@/lib/utils';
import type { Message } from '@/features/messages/types';
import {canonicalWorkStatus, dueFilters, filterAndSortWork, validDeadline, workDueBucket, workDueCounts, type Collab, type DueFilter, type TeamItem, type WorkOrder} from './deadlines';
export {canonicalWorkStatus} from './deadlines';
import './work.css';
type Work = {
    important: Message[];
    snoozed_due: Message[];
    attention: Message[];
    reference: Message[];
    counts: Record<string, number>;
};
export const statuses = [{ id: 'new', label: '새로 접수' }, { id: 'needs_action', label: '조치 필요' }, { id: 'in_progress', label: '진행 중' }, { id: 'waiting', label: '대기' }, { id: 'done', label: '완료' }];
export function WorkPage() {
    const [signals, setSignals] = useState(false);
    return <><div className="page workflow-switch"><div className="tabs" role="group" aria-label="업무 보기"><button aria-pressed={!signals} className={!signals ? 'active' : ''} onClick={() => setSignals(false)}>업무 상태</button><button aria-pressed={signals} className={signals ? 'active' : ''} onClick={() => setSignals(true)}>메일 신호</button></div></div>{signals ? <SignalsPage/> : <TeamPage title="내 업무"/>}</>;
}
function SignalsPage() {
    const work = useQuery({ queryKey: ['work'], queryFn: ({ signal }) => api<Work>('/api/work-inbox?limit=200', { signal }) });
    return <div className="page"><PageHeader title="내 업무" description="중요도와 메일 신호를 모아, 지금 필요한 일을 먼저 확인하세요." actions={<Button variant="outline" onClick={() => work.refetch()}><RefreshCw size={16}/>새로고침</Button>}/>{work.isPending ? <Loading /> : work.error ? <ErrorState error={work.error} retry={() => work.refetch()}/> : <div className="kanban work-board">{([{ key: 'snoozed_due', label: '다시 확인할 메일', color: 'amber' }, { key: 'important', label: '중요 업무', color: 'blue' }, { key: 'attention', label: '확인이 필요한 첨부', color: 'violet' }, { key: 'reference', label: '참고 메일', color: 'slate' }] as const).map(bucket => <section className="kanban-column" key={bucket.key}><h2><span className={`status-dot ${bucket.color}`}/>{bucket.label}<span className="muted">{work.data?.[bucket.key]?.length ?? 0}</span></h2>{!(work.data?.[bucket.key]?.length) ? <p className="column-empty">해당 업무가 없습니다</p> : work.data[bucket.key].map(message => <div className="work-item" key={message.id}><Link to={`/mail?message=${encodeURIComponent(message.id)}`}><small className="muted">{message.from.name || message.from.email}</small><h3>{message.subject || '(제목 없음)'}</h3></Link><div className="row between"><small className="muted">{formatDate(message.date)}</small><Link className="small" to={`/team?message=${encodeURIComponent(message.id)}`}>처리 관리 →</Link></div></div>)}</section>)}</div>}<p className="small muted">최근 받은메일 최대 200건의 신호를 분류합니다. 실제 처리 상태와 담당자는 팀 업무함에서 관리합니다.</p></div>;
}
export function TeamPage({title = '팀 업무함'}: {title?: string} = {}) {
    const user = useSession();
    const [assignee, setAssignee] = useState('');
    const [params, setParams] = useSearchParams();
    const selected = params.get('message') || '';
    const setSelected = (id: string) => { const next = new URLSearchParams(params); if (id)
        next.set('message', id);
    else
        next.delete('message'); setParams(next); };
    const [filter, setFilter] = useState('');
    const [dueFilter, setDueFilter] = useState<DueFilter>('all');
    const [order, setOrder] = useState<WorkOrder>('recent');
    const [now, setNow] = useState(() => new Date());
    const boardID = useId();
    useEffect(() => {
        const tick = () => setNow(new Date());
        const timer = window.setInterval(tick, 30000);
        window.addEventListener('focus', tick);
        return () => {window.clearInterval(timer); window.removeEventListener('focus', tick)};
    }, []);
    const team = useQuery({ queryKey: ['team', assignee, filter], queryFn: ({ signal }) => api<{
            items: TeamItem[];
        }>(`/api/team-inbox?limit=200&assignee=${encodeURIComponent(assignee)}&status=${encodeURIComponent(filter)}`, { signal }) });
    const loadedItems = team.data?.items || [];
    const counts = workDueCounts(loadedItems, now);
    const visibleItems = filterAndSortWork(loadedItems, dueFilter, order, now);
    const visibleStatuses = dueFilter !== 'all' || filter ? statuses.filter(status => visibleItems.some(item => canonicalWorkStatus(item.collab.status) === status.id)) : statuses;
    const filtered = !!assignee || !!filter || dueFilter !== 'all';
    const resetFilters = () => {setAssignee(''); setFilter(''); setDueFilter('all'); setOrder('recent')};
    return <div className="page"><PageHeader title={title} description="내 소유 메일에 등록한 업무를 다섯 단계로 관리합니다. 관리자도 다른 사용자의 메일은 볼 수 없습니다." actions={<Button variant="outline" disabled={team.isFetching} onClick={() => {setNow(new Date()); void team.refetch()}}><RefreshCw size={16}/>새로고침</Button>}/>
        <div className="row filters work-filters"><Input aria-label="담당자 필터" placeholder="담당자 이름 또는 아이디" value={assignee} onChange={event => setAssignee(event.target.value)}/><Button variant="secondary" disabled={!user?.login_id} onClick={() => setAssignee(user?.login_id || '')}>내 담당</Button><select className="input" aria-label="상태 필터" value={filter} onChange={event => setFilter(event.target.value)}><option value="">모든 상태</option>{statuses.map(status => <option key={status.id} value={status.id}>{status.label}</option>)}</select><select className="input" aria-label="업무 정렬" value={order} onChange={event => setOrder(event.target.value as WorkOrder)}><option value="recent">최근 변경순</option><option value="deadline">기한 빠른순</option></select><Button variant="ghost" disabled={!filtered && order === 'recent'} onClick={resetFilters}>필터 초기화</Button></div>
        <div className="work-due-filters" role="group" aria-label="업무 기한 필터">{dueFilters.map(option => <Button key={option.id} variant={dueFilter === option.id ? 'secondary' : 'outline'} aria-pressed={dueFilter === option.id} disabled={team.isPending || !!team.error} onClick={() => {setNow(new Date()); setDueFilter(option.id)}}>{option.label}<span className="work-filter-count">{team.isPending || team.error ? '—' : counts[option.id]}</span></Button>)}</div>
        <p className="small muted work-scope">현재 담당자·상태 조건에서 최근 변경된 최대 200건만 불러옵니다. 기한별 건수와 정렬도 이 범위에만 적용됩니다.</p>
        <p className="small muted work-scope">지연은 현재 시각을 지난 업무, 오늘은 내 현지 날짜의 남은 기한입니다. 완료 업무는 전체에만 표시하고 최근 변경순을 유지합니다.</p>
        {!team.isPending && !team.error && <p className="small work-result-count" role="status">표시 {visibleItems.length} / 불러온 {loadedItems.length}건 · 완료 {counts.completed}건</p>}
        <div className={`team-layout workflow-layout ${selected ? 'with-detail' : ''}`}><div>{team.isPending ? <Loading/> : team.error ? <ErrorState error={team.error} retry={() => team.refetch()}/> : !visibleItems.length ? <EmptyState title={filtered ? '조건에 맞는 업무가 없습니다' : '등록된 처리 업무가 없습니다'} description={filtered ? '현재 불러온 범위에서 조건에 맞는 업무가 없습니다. 기한·담당자·상태 조건을 바꿔 보세요.' : '메일 신호나 원본 메일의 처리 관리에서 상태 또는 담당자를 지정하세요.'} action={filtered ? <Button variant="outline" onClick={resetFilters}>전체 업무 보기</Button> : undefined}/> : <><p id={`${boardID}-help`} className="small muted work-board-help">Tab으로 업무를 선택하고 Enter로 처리 정보를 여세요. 넓은 보드에 초점을 두면 ← / → 키로 가로 이동할 수 있습니다.</p><div className="workflow-board" role="region" aria-label="업무 상태 보드" aria-describedby={`${boardID}-help`} tabIndex={0} onKeyDown={event => {if (event.target !== event.currentTarget || !['ArrowLeft', 'ArrowRight'].includes(event.key)) return; event.preventDefault(); event.currentTarget.scrollBy({left: event.key === 'ArrowLeft' ? -260 : 260, behavior: 'auto'})}}>{visibleStatuses.map(status => {
            const items = visibleItems.filter(item => canonicalWorkStatus(item.collab.status) === status.id);
            return <section className="kanban-column" key={status.id} aria-label={status.label}><h2><span className={`status-dot ${status.id === 'done' ? 'green' : status.id === 'waiting' ? 'violet' : status.id === 'needs_action' ? 'amber' : 'blue'}`}/>{status.label}<span className="muted">{items.length}</span></h2>{items.length ? items.map(item => <button type="button" className={`work-item work-select ${selected === item.collab.message_id ? 'selected' : ''}`} aria-pressed={selected === item.collab.message_id} aria-controls={selected === item.collab.message_id ? `${boardID}-detail` : undefined} key={item.collab.message_id} onClick={() => setSelected(item.collab.message_id)}><small className="muted">{item.message?.from.name || item.message?.from.email}</small><h3>{item.message?.subject || '메일 업무'}</h3><div className="row small"><UserRound size={13}/>{item.collab.assignee || '미배정'}</div>{validDeadline(item.collab.sla_due) ? <div className={`row small ${workDueBucket(item, now) === 'overdue' ? 'danger-text' : 'muted'}`}><Clock size={13}/>{workDueBucket(item, now) === 'overdue' && <span>기한 지남 · </span>}{formatDate(item.collab.sla_due)}</div> : <div className="row small muted"><Clock size={13}/>기한 없음</div>}</button>) : <p className="column-empty">해당 업무가 없습니다</p>}</section>
        })}</div></>}</div>{selected && <CollaborationPanel key={selected} id={selected} panelID={`${boardID}-detail`} onClose={() => setSelected('')}/>}</div>
    </div>;
}
function CollaborationPanel({ id, panelID, onClose }: {
    id: string;
    panelID: string;
    onClose: () => void;
}) {
    const cache = useQueryClient();
    const data = useQuery({ queryKey: ['collab', id], queryFn: ({ signal }) => api<{
            collab: Collab;
            notes: {
                id: string;
                author: string;
                body: string;
                created_at: number;
            }[];
        }>(`/api/messages/${encodeURIComponent(id)}/collab`, { signal }) });
    const [assignee, setAssignee] = useState('');
    const [due, setDue] = useState('');
    const [note, setNote] = useState('');
    const mutation = useMutation({ mutationFn: ({ operation, body }: {
            operation: string;
            body: unknown;
        }) => api(`/api/messages/${encodeURIComponent(id)}/${operation}`, { method: 'POST', body }), onSuccess: (_, variables) => {
            void cache.invalidateQueries({ queryKey: ['team'] });
            void cache.invalidateQueries({ queryKey: ['collab', id] });
            // A status/assignee request may finish while a person starts typing a
            // note. Only clear the exact note that was successfully submitted.
            if (variables.operation === 'notes')
                setNote(value => value === (variables.body as {
                    body: string;
                }).body ? '' : value);
            toast.success('처리 정보를 저장했습니다.');
        }, onError: error => toast.error(error.message) });
    function submit(event: FormEvent, operation: string, body: unknown) { event.preventDefault(); mutation.mutate({ operation, body }); }
    return <aside className="work-detail" id={panelID} aria-label="업무 처리 관리"><div className="row between"><h2>처리 관리</h2><Button variant="ghost" size="sm" onClick={onClose}>닫기</Button></div><Link to={`/mail?message=${encodeURIComponent(id)}`}>원본 메일 열기 →</Link>{data.isPending ? <Loading/> : data.error ? <ErrorState error={data.error}/> : <div className="stack">
      <label className="field">처리 상태<select className="input" value={canonicalWorkStatus(data.data.collab.status)} disabled={mutation.isPending} onChange={event => mutation.mutate({operation: 'work-status', body: {status: event.target.value}})}>{statuses.map(status => <option key={status.id} value={status.id}>{status.label}</option>)}</select></label>
      <form className="stack" onSubmit={event => submit(event, 'assign', {assignee})}><label className="field">담당자 <span className="muted">현재: {data.data.collab.assignee || '미배정'}</span><Input value={assignee} onChange={event => setAssignee(event.target.value)} placeholder="담당자 아이디 (빈 값은 해제)"/></label><Button variant="outline" type="submit" disabled={mutation.isPending}>담당 저장</Button></form>
      <form className="stack" onSubmit={event => submit(event, 'sla', {sla_due: due ? Math.floor(new Date(due).getTime()/1000) : 0})}><label className="field">처리 기한 <span className="muted">현재: {formatDate(data.data.collab.sla_due)}</span><Input type="datetime-local" value={due} onChange={event => setDue(event.target.value)}/></label><Button variant="outline" type="submit" disabled={mutation.isPending}>기한 저장 (빈 값은 해제)</Button></form>
      <Link to={`/actions?message=${encodeURIComponent(id)}&create=1`}>이 메일에서 액션 만들기 →</Link><h3 className="row"><MessageSquare size={16}/>내부 메모</h3><div className="notes-list">{data.data.notes?.map(item => <div key={item.id}><strong>{item.author}</strong><small className="muted"> · {formatDate(item.created_at)}</small><p className="pre-wrap">{item.body}</p></div>)}</div>
      <form className="stack" onSubmit={event => submit(event, 'notes', {body: note})}><Textarea aria-label="내부 메모" value={note} onChange={event => setNote(event.target.value)} required placeholder="메일로 발송되지 않는 내부 메모"/><Button type="submit" disabled={mutation.isPending || !note.trim()}>메모 추가</Button></form>
    </div>}</aside>;
}
