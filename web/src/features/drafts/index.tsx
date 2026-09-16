import {useState} from 'react'
import {Link} from 'react-router-dom'
import {useInfiniteQuery, useMutation, useQueryClient} from '@tanstack/react-query'
import {Plus, RefreshCw, Trash2} from 'lucide-react'
import {z} from 'zod'
import {api} from '@/api/client'
import {nullableList, parseResponse} from '@/api/response'
import {Badge, Button, EmptyState, ErrorState, Loading, PageHeader} from '@/components/ui'
import {addresses, mailDate} from '@/features/messages/types'

// Go slices can be null on the wire. Normalize only this legitimate empty
// representation; malformed pages/items must remain visible query failures.
const draftPageSchema = z.object({
  drafts: nullableList(z.object({id: z.string(), account_id: z.string(), status: z.string(), current_version: z.number(), updated_at: z.number(), subject: z.string(), author: z.string(),
    to: nullableList(z.object({email: z.string(), name: z.string().optional()})),
  })),
  next_cursor: z.string().optional(),
})
function draftPage(value: unknown) {
  return parseResponse(draftPageSchema, value)
}

export function DraftsPage() {
  const cache = useQueryClient()
  const [status, setStatus] = useState('open')
  const drafts = useInfiniteQuery({queryKey: ['drafts', status], initialPageParam: '', queryFn: async ({pageParam, signal}) => draftPage(await api<unknown>(`/api/drafts?status=${status}&limit=50&cursor=${encodeURIComponent(pageParam)}`, {signal})), getNextPageParam: page => page.next_cursor || undefined})
  const discard = useMutation({mutationFn: (id: string) => api(`/api/drafts/${encodeURIComponent(id)}`, {method: 'DELETE'}), onSuccess: (_, id) => {cache.removeQueries({queryKey: ['draft', id]}); void cache.invalidateQueries({queryKey: ['drafts']})}})
  const rows = drafts.data?.pages.flatMap(page => page.drafts) ?? []
  return <div className="page"><PageHeader title="저장된 초안" description="서버에 저장된 내 초안입니다. 다른 브라우저에서도 이어서 작성할 수 있습니다." actions={<><Button variant="outline" onClick={() => drafts.refetch()}><RefreshCw size={16}/>새로고침</Button><Button asChild><Link to="/compose"><Plus size={16}/>새 메일 작성</Link></Button></>}/>
    <div className="tabs">{[{id: 'open', name: '작성 중'}, {id: 'approved', name: '승인됨'}, {id: 'discarded', name: '삭제된 초안'}].map(item => <button key={item.id} className={status === item.id ? 'active' : ''} onClick={() => setStatus(item.id)}>{item.name}</button>)}</div>
    {discard.error && <ErrorState error={discard.error}/>}
    {drafts.isPending ? <Loading/> : drafts.error ? <ErrorState error={drafts.error} retry={() => drafts.refetch()}/> : !rows.length ? <EmptyState title="저장된 초안이 없습니다" description="메일을 작성하고 저장하면 이곳에 표시됩니다."/> : rows.map(item => <article className="outbound-item" key={item.id}><div><h2><Link to={`/drafts/${encodeURIComponent(item.id)}`}>{item.subject || '(제목 없음)'}</Link></h2><p className="muted">{addresses(item.to) || '수신자 미지정'}</p><small className="muted">{mailDate(item.updated_at)} · v{item.current_version}</small></div><div className="row"><Badge variant="outline">{item.author === 'ai' ? 'AI 작성' : '직접 작성'}</Badge>{['open', 'approved'].includes(item.status) && <Button variant="ghost" size="icon" disabled={discard.isPending} aria-label={`${item.subject || '제목 없는'} 초안 삭제`} onClick={() => {if (window.confirm('이 초안을 삭제하시겠습니까? 발송되지 않으며 기존 메일 원본은 유지됩니다.')) discard.mutate(item.id)}}><Trash2 size={16}/></Button>}</div></article>)}
    {drafts.hasNextPage && <Button variant="outline" disabled={drafts.isFetchingNextPage} onClick={() => drafts.fetchNextPage()}>초안 더 보기</Button>}
    {status === 'discarded' && <p className="small muted">삭제된 초안은 감사 목적으로 보존되며 발송할 수 없습니다.</p>}
  </div>
}
