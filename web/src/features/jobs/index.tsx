import {Link, useParams} from 'react-router-dom'
import {useMutation, useQuery, useQueryClient} from '@tanstack/react-query'
import {z} from 'zod'
import {api} from '@/api/client'
import {nullableList, parseResponse} from '@/api/response'
import {Badge, Button, EmptyState, ErrorState, Loading, PageHeader, Panel} from '@/components/ui'
import {formatDate} from '@/lib/utils'
const jobSchema = z.object({id:z.string(),account_id:z.string().optional(),type:z.string(),status:z.string(),progress:z.string().optional(),error:z.string().optional(),stats:z.record(z.string(),z.number()).nullable().optional(),created_at:z.number()})
const labels: Record<string, string> = {queued: '대기', running: '진행 중', succeeded: '완료', partially_succeeded: '일부 완료', failed: '실패', cancelled: '취소'}
export function JobsPage() {
  const {id} = useParams()
  const cache = useQueryClient()
  const jobs = useQuery({queryKey: ['jobs', id || 'list'], queryFn: async ({signal}) => id ? [parseResponse(jobSchema, await api<unknown>(`/api/jobs/${encodeURIComponent(id)}`, {signal}))] : parseResponse(nullableList(jobSchema), await api<unknown>('/api/jobs?limit=100', {signal})), refetchInterval: query => query.state.data?.some(job => ['queued', 'running'].includes(job.status)) ? 2000 : false})
  const cancel = useMutation({mutationFn: (jobID: string) => api(`/api/jobs/${encodeURIComponent(jobID)}/cancel`, {method: 'POST'}), onSuccess: () => cache.invalidateQueries({queryKey: ['jobs']})})
  return <div className="page"><PageHeader title={id ? '작업 상태' : '작업 내역'} description="동기화와 백그라운드 작업의 진행 및 결과를 확인합니다." actions={<Button variant="outline" onClick={() => jobs.refetch()}>새로고침</Button>}/>{cancel.error && <ErrorState error={cancel.error}/>}{jobs.isPending ? <Loading/> : jobs.error ? <ErrorState error={jobs.error} retry={() => jobs.refetch()}/> : !jobs.data?.length ? <EmptyState title="표시할 작업이 없습니다"/> : jobs.data.map(job => <Panel key={job.id}><div className="row between"><h2><Link to={`/jobs/${encodeURIComponent(job.id)}`}>{job.type || '메일 작업'}</Link></h2><Badge variant={job.status === 'failed' ? 'destructive' : 'outline'}>{labels[job.status] || job.status}</Badge></div><p>{job.progress || '진행 정보 없음'}</p><p className="small muted">{formatDate(job.created_at)} · {job.id}</p>{job.error && <ErrorState error={new Error(job.error)}/>}<dl className="grid">{Object.entries(job.stats ?? {}).map(([name, value]) => <div key={name}><dt>{({new: '신규', duplicate: '중복', failed: '실패', oversize: '크기 초과'} as Record<string,string>)[name] || name}</dt><dd>{value}</dd></div>)}</dl><div className="row">{job.account_id && <Button variant="ghost" asChild><Link to={`/accounts/${encodeURIComponent(job.account_id)}`}>메일 계정 열기</Link></Button>}{['queued', 'running'].includes(job.status) && <Button variant="outline" disabled={cancel.isPending} onClick={() => {if (window.confirm('이 작업을 취소하시겠습니까? 이미 수집된 메일은 유지됩니다.')) cancel.mutate(job.id)}}>작업 취소</Button>}</div></Panel>)}</div>
}
