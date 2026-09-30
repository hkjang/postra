import {Link, useParams} from 'react-router-dom'
import {useMutation, useQuery, useQueryClient} from '@tanstack/react-query'
import {z} from 'zod'
import {api} from '@/api/client'
import {nullableList, parseResponse} from '@/api/response'
import {Badge, Button, EmptyState, ErrorState, Loading, PageHeader, Panel} from '@/components/ui'
import {formatDate} from '@/lib/utils'
import './jobs.css'
// The server renders the diagnostic's explanation from its own vocabulary and
// re-checks every field on read, so the client shows that sentence as given and
// adds only the numbers an operator copies into a ticket.
const diagnosticSchema = z.object({
  summary: z.string().optional(), command: z.string().optional(), code: z.string().optional(),
  elapsed_ms: z.number().optional(), timeout_ms: z.number().optional(), attempts: z.number().optional(),
  host: z.string().optional(), port: z.number().optional(), protocol: z.string().optional(),
  host_sessions: z.number().optional(), account_sessions: z.number().optional(), slot_wait_ms: z.number().optional(),
})
const jobSchema = z.object({id:z.string(),account_id:z.string().optional(),type:z.string(),status:z.string(),progress:z.string().optional(),error:z.string().optional(),stats:z.record(z.string(),z.number()).nullable().optional(),diagnostic:diagnosticSchema.nullable().optional(),created_at:z.number()})
type Diagnostic = z.output<typeof diagnosticSchema>
const labels: Record<string, string> = {queued: '대기', running: '진행 중', succeeded: '완료', partially_succeeded: '일부 완료', failed: '실패', cancelled: '취소'}
const seconds = (ms: number) => ms < 1000 ? `${ms}ms` : `${(ms / 1000).toFixed(1)}초`

// SyncDiagnostic shows where a sync stopped. A failed sync is the most serious
// thing this service reports, and "response timed out" alone cannot be checked
// by anyone: the step, its own deadline, the retries and the connections this
// node held are what turn it into an action.
function SyncDiagnostic({diagnostic, failed}: {diagnostic: Diagnostic; failed: boolean}) {
  const rows: [string, string][] = []
  if (diagnostic.host) rows.push(['서버', `${diagnostic.protocol?.toUpperCase() ?? ''} ${diagnostic.host}${diagnostic.port ? `:${diagnostic.port}` : ''}`.trim()])
  if (diagnostic.command) rows.push(['명령', diagnostic.command])
  if (diagnostic.code) rows.push(['서버 응답 코드', diagnostic.code])
  if (diagnostic.elapsed_ms) rows.push(['소요 시간', seconds(diagnostic.elapsed_ms)])
  if (diagnostic.timeout_ms) rows.push(['이 단계의 제한 시간', seconds(diagnostic.timeout_ms)])
  if (diagnostic.attempts && diagnostic.attempts > 1) rows.push(['연결 시도', `${diagnostic.attempts}회`])
  if (diagnostic.host_sessions) rows.push(['이 서버에 유지 중인 연결', `${diagnostic.host_sessions}개${diagnostic.account_sessions ? ` (이 계정 ${diagnostic.account_sessions}개)` : ''}`])
  if (diagnostic.slot_wait_ms && diagnostic.slot_wait_ms >= 1000) rows.push(['시작 전 대기', seconds(diagnostic.slot_wait_ms)])
  if (!diagnostic.summary && rows.length === 0) return null
  return <section className="job-diagnostic stack" role="group" aria-label="동기화 진단">
    <h3>{failed ? '멈춘 지점' : '동기화 진단'}</h3>
    {diagnostic.summary && <p>{diagnostic.summary}</p>}
    {rows.length > 0 && <dl className="grid">{rows.map(([name, value]) => <div key={name}><dt>{name}</dt><dd>{value}</dd></div>)}</dl>}
    <p className="small muted">이 내용은 Postra가 기록한 단계·시간·횟수이며 메일 서버가 보낸 문장은 포함하지 않습니다. 문의 시 작업 ID와 함께 전달하세요.</p>
  </section>
}

export function JobsPage() {
  const {id} = useParams()
  const cache = useQueryClient()
  const jobs = useQuery({queryKey: ['jobs', id || 'list'], queryFn: async ({signal}) => id ? [parseResponse(jobSchema, await api<unknown>(`/api/jobs/${encodeURIComponent(id)}`, {signal}))] : parseResponse(nullableList(jobSchema), await api<unknown>('/api/jobs?limit=100', {signal})), refetchInterval: query => query.state.data?.some(job => ['queued', 'running'].includes(job.status)) ? 2000 : false})
  const cancel = useMutation({mutationFn: (jobID: string) => api(`/api/jobs/${encodeURIComponent(jobID)}/cancel`, {method: 'POST'}), onSuccess: () => cache.invalidateQueries({queryKey: ['jobs']})})
  return <div className="page"><PageHeader title={id ? '작업 상태' : '작업 내역'} description="동기화와 백그라운드 작업의 진행 및 결과를 확인합니다." actions={<Button variant="outline" onClick={() => jobs.refetch()}>새로고침</Button>}/>{cancel.error && <ErrorState error={cancel.error}/>}{jobs.isPending ? <Loading/> : jobs.error ? <ErrorState error={jobs.error} retry={() => jobs.refetch()}/> : !jobs.data?.length ? <EmptyState title="표시할 작업이 없습니다"/> : jobs.data.map(job => <Panel key={job.id}><div className="row between"><h2><Link to={`/jobs/${encodeURIComponent(job.id)}`}>{job.type || '메일 작업'}</Link></h2><Badge variant={job.status === 'failed' ? 'destructive' : job.status === 'partially_succeeded' ? 'secondary' : 'outline'}>{labels[job.status] || job.status}</Badge></div><p>{job.progress || '진행 정보 없음'}</p><p className="small muted">{formatDate(job.created_at)} · {job.id}</p>{job.error && <ErrorState error={new Error(job.error)}/>}{job.diagnostic && <SyncDiagnostic diagnostic={job.diagnostic} failed={['failed', 'partially_succeeded'].includes(job.status)}/>}<dl className="grid">{Object.entries(job.stats ?? {}).map(([name, value]) => <div key={name}><dt>{({new: '신규', duplicate: '중복', failed: '실패', oversize: '크기 초과', seen: '확인', parse_error: '분석 오류', sent_seen: '보낸 메일 확인', sent_new: '보낸 메일 신규', sent_duplicate: '보낸 메일 중복', sent_failed: '보낸 메일 실패'} as Record<string,string>)[name] || name}</dt><dd>{value}</dd></div>)}</dl><div className="row">{job.account_id && <Button variant="ghost" asChild><Link to={`/accounts/${encodeURIComponent(job.account_id)}`}>메일 계정 열기</Link></Button>}{['queued', 'running'].includes(job.status) && <Button variant="outline" disabled={cancel.isPending} onClick={() => {if (window.confirm('이 작업을 취소하시겠습니까? 이미 수집된 메일은 유지됩니다.')) cancel.mutate(job.id)}}>작업 취소</Button>}</div></Panel>)}</div>
}
