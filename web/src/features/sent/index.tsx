import {Link} from 'react-router-dom'
import {useQuery} from '@tanstack/react-query'
import {RefreshCw} from 'lucide-react'
import {z} from 'zod'
import {api} from '@/api/client'
import {nullableList, parseResponse} from '@/api/response'
import {Badge, Button, EmptyState, ErrorState, Loading, PageHeader} from '@/components/ui'
import {formatDate} from '@/lib/utils'
const outboundSchema = z.object({outbound:z.object({id:z.string(),draft_id:z.string(),status:z.string(),smtp_response:z.string().optional(),attempts:z.number(),created_at:z.number(),next_attempt_at:z.number().optional()}),subject:z.string().optional(),to:nullableList(z.string()).optional()})
const labels: Record<string, string> = {sent: '발송 완료', queued: '대기', sending: '발송 중', failed: '실패', retry_wait: '재시도 대기', send_uncertain: '결과 확인 필요'}
export function SentPage() {
  const out = useQuery({queryKey: ['outbound'], queryFn: async ({signal}) => parseResponse(nullableList(outboundSchema),await api<unknown>('/api/outbound?limit=100', {signal})), refetchInterval: query => query.state.data?.some(item => ['queued', 'sending', 'retry_wait'].includes(item.outbound.status)) ? 5000 : false})
  return <div className="page"><PageHeader title="발송 메일" description="승인한 메일의 SMTP 발송 결과와 재시도 상태를 확인합니다." actions={<Button variant="outline" onClick={() => out.refetch()}><RefreshCw size={16}/>새로고침</Button>}/>{out.isPending ? <Loading/> : out.error ? <ErrorState error={out.error} retry={() => out.refetch()}/> : !out.data?.length ? <EmptyState title="아직 발송한 메일이 없습니다" description="새 메일을 작성하고 미리보기에서 승인하면 발송됩니다."/> : out.data.map(({outbound: item, subject, to}) => <article className="outbound-item" key={item.id}><div><h2><Link to={`/drafts/${encodeURIComponent(item.draft_id)}`}>{subject || '발송한 초안 열기'}</Link></h2><p className="muted">{to?.join(', ')}</p><p className="muted">{formatDate(item.created_at)} · 시도 {item.attempts}회</p>{item.smtp_response && <p>{item.smtp_response}</p>}{item.status === 'send_uncertain' && <p className="danger-text">전달 여부가 불확실합니다. 중복 발송을 피하려면 서버나 수신자에게 확인하세요.</p>}{!!item.next_attempt_at && <p className="muted">다음 시도: {formatDate(item.next_attempt_at)}</p>}<p className="muted">추적 ID: {item.id}</p></div><Badge variant={item.status === 'sent' ? 'default' : ['failed', 'send_uncertain'].includes(item.status) ? 'destructive' : 'secondary'}>{labels[item.status] || item.status}</Badge></article>)}</div>
}
