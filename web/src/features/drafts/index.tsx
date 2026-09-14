import {useState, type FormEvent} from 'react'
import {Link, useNavigate} from 'react-router-dom'
import {useQueryClient} from '@tanstack/react-query'
import {FilePenLine, Plus} from 'lucide-react'
import {Badge, Button, EmptyState, Input, PageHeader} from '@/components/ui'
import type {DraftView} from '@/features/messages/types'

// The existing Go API only supports draft-by-ID. Until a scoped list endpoint
// is approved, never imply that a browser query cache is the full draft store.
export function DraftsPage() {
  const cache = useQueryClient()
  const navigate = useNavigate()
  const [id, setID] = useState('')
  const drafts = cache.getQueriesData<DraftView>({queryKey: ['draft']}).map(([, value]) => value).filter((value): value is DraftView => !!value && value.draft.status === 'open')
  function open(event: FormEvent) {event.preventDefault(); if (id.trim()) navigate('/drafts/' + encodeURIComponent(id.trim()))}
  return <div className="page"><PageHeader title="최근 작업한 초안" description="현재 브라우저 세션에서 열거나 저장한 초안입니다. 서버의 전체 초안 목록은 아닙니다." actions={<Button asChild><Link to="/compose"><Plus size={16}/>새 메일 작성</Link></Button>}/>{drafts.length ? drafts.map(view => <article className="outbound-item" key={view.draft.id}><div><h2><Link to={`/drafts/${view.draft.id}`}>{view.version.subject || '(제목 없음)'}</Link></h2><p className="muted">{view.version.to?.map(address => address.email).join(', ') || '수신자 미지정'}</p><small className="muted">{view.draft.id}</small></div><Badge variant="outline">{view.version.author === 'ai' ? 'AI 작성' : '작성 중'} · v{view.version.version}</Badge></article>) : <EmptyState title="이 세션에서 작업한 초안이 없습니다" description="새 메일을 저장하거나 기존 초안 ID로 이어서 작성할 수 있습니다."/>}<form className="row filters" onSubmit={open} style={{marginTop: 24}}><Input aria-label="기존 초안 ID" value={id} onChange={event => setID(event.target.value)} placeholder="기존 초안 ID로 열기" required/><Button variant="outline" type="submit"><FilePenLine size={16}/>초안 열기</Button></form><p className="small muted">초안 내용은 서버에 저장됩니다. 새로고침하면 이 최근 목록은 초기화되지만 초안 자체가 삭제되는 것은 아닙니다.</p></div>
}
