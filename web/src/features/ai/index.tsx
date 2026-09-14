import {useState, type FormEvent} from 'react'
import {Link, useSearchParams} from 'react-router-dom'
import {useMutation, useQuery} from '@tanstack/react-query'
import {ArrowUp, FileText, Sparkles} from 'lucide-react'
import {z} from 'zod'
import {api} from '@/api/client'
import {Button, EmptyState, ErrorState, Loading, PageHeader, Textarea} from '@/components/ui'
import type {Account, Analysis} from '@/features/messages/types'

type Answer = {answer?: string; evidence_message_ids?: string[]; confidence?: number; headline?: string; needs_reply?: {message_id: string; who: string; what: string}[]; deadlines?: {message_id: string; what: string; when: string}[]; fyi?: string[]; volume_note?: string}
const answerSchema = z.object({
  answer: z.string().optional().catch(undefined), evidence_message_ids: z.array(z.string()).optional().catch(undefined),
  confidence: z.number().min(0).max(1).optional().catch(undefined), headline: z.string().optional().catch(undefined),
  needs_reply: z.array(z.object({message_id: z.string(), who: z.string(), what: z.string()})).optional().catch(undefined),
  deadlines: z.array(z.object({message_id: z.string(), what: z.string(), when: z.string()})).optional().catch(undefined),
  fyi: z.array(z.string()).optional().catch(undefined), volume_note: z.string().optional().catch(undefined),
})
export function resultOf(analysis?: Analysis): Answer | undefined {
  if (!analysis) return
  try {const parsed = answerSchema.safeParse(JSON.parse(analysis.result_json)); return parsed.success ? parsed.data : {answer: 'AI 응답 형식을 확인할 수 없습니다. 다시 생성해 주세요.'}}
  catch {return {answer: analysis.result_json}}
}
function AccountSelect({value, onChange}: {value: string; onChange: (value: string) => void}) {
  const accounts = useQuery({queryKey: ['accounts'], queryFn: () => api<Account[]>('/api/accounts')})
  if (accounts.error) return <ErrorState error={accounts.error} retry={() => accounts.refetch()}/>
  return <label className="row small muted">대상 계정<select className="input" style={{width: 'auto', maxWidth: '100%'}} value={value} onChange={event => onChange(event.target.value)}><option value="">내 모든 계정</option>{accounts.data?.map(account => <option key={account.id} value={account.id}>{account.email}</option>)}</select></label>
}
export function AskPage() {
  const [params] = useSearchParams()
  const [question, setQuestion] = useState(params.get('q') || '')
  const [account, setAccount] = useState('')
  const answer = useMutation({mutationFn: () => api<Analysis>('/api/qa', {body: {question, account_id: account}})})
  const result = resultOf(answer.data)
  function ask(event: FormEvent) {event.preventDefault(); if (question.trim()) answer.mutate()}
  return <div className="page ai-page"><div className="ai-hero"><Sparkles size={32}/><h1>Ask Postra</h1><p className="muted">내 메일을 근거로, 업무의 맥락을 찾아보세요.</p></div><form className="stack" onSubmit={ask}><div className="ai-question"><Textarea aria-label="AI에게 질문" value={question} onChange={event => setQuestion(event.target.value)} placeholder="지난주 계약 검토와 관련해 회신해야 할 내용을 알려줘" required/><Button type="submit" disabled={answer.isPending || !question.trim()}><ArrowUp size={17}/>{answer.isPending ? '근거를 찾는 중…' : '질문하기'}</Button></div><AccountSelect value={account} onChange={setAccount}/></form>{answer.isPending && <Loading label="내 메일에서 답변의 근거를 찾고 있습니다…"/>}{answer.error && <ErrorState error={answer.error}/>}<p className="small muted">질문을 실행하면 설정된 AI가 접근 가능한 메일을 분석합니다. 답변은 원본 메일과 확인해 주세요.</p>{result && <article className="ai-result"><h2><Sparkles size={18}/>메일에서 찾은 답변</h2><p className="pre-wrap">{result.answer || '답변을 생성하지 못했습니다.'}</p>{!!result.evidence_message_ids?.length && <><h3>근거 메일</h3><div className="evidence-list">{result.evidence_message_ids.map((id, index) => <Link key={id} to={`/mail?message=${encodeURIComponent(id)}`}><FileText size={12}/>근거 {index + 1}</Link>)}</div></>}<p className="small muted">{answer.data?.model}{typeof result.confidence === 'number' ? ` · 신뢰도 ${Math.round(result.confidence * 100)}%` : ''}</p></article>}</div>
}
export function DigestPage() {
  const [account, setAccount] = useState('')
  const [hours, setHours] = useState(24)
  const digest = useMutation({mutationFn: () => api<Analysis>('/api/digest', {body: {account_id: account, hours}})})
  const result = resultOf(digest.data)
  return <div className="page ai-page"><PageHeader title="메일 브리핑" description="회신할 메일, 놓치기 쉬운 기한, 참고 소식을 한 번에 확인하세요."/><div className="row filters"><AccountSelect value={account} onChange={setAccount}/><select className="input" aria-label="브리핑 기간" value={hours} onChange={event => setHours(Number(event.target.value))}><option value={24}>최근 24시간</option><option value={72}>최근 3일</option><option value={168}>최근 7일</option></select><Button disabled={digest.isPending} onClick={() => digest.mutate()}><Sparkles size={16}/>{digest.isPending ? '브리핑 생성 중…' : '브리핑 생성'}</Button></div>{digest.isPending ? <Loading label="받은 메일을 정리하고 있습니다…"/> : digest.error ? <ErrorState error={digest.error}/> : !result ? <EmptyState title="오늘의 업무 흐름을 정리해 보세요" description="선택한 기간의 내 메일을 바탕으로 브리핑을 생성합니다."/> : <article className="ai-result"><h2>{result.headline || '메일 브리핑'}</h2><h3>회신이 필요한 메일</h3>{result.needs_reply?.length ? <ul>{result.needs_reply.map((item, index) => <li key={index}><Link to={`/mail?message=${encodeURIComponent(item.message_id)}`}>{item.who}</Link> · {item.what}</li>)}</ul> : <p className="muted">확인된 회신 요청이 없습니다.</p>}<h3>확인할 기한</h3>{result.deadlines?.length ? <ul>{result.deadlines.map((item, index) => <li key={index}><strong>{item.when}</strong> · <Link to={`/mail?message=${encodeURIComponent(item.message_id)}`}>{item.what}</Link></li>)}</ul> : <p className="muted">확인된 기한이 없습니다.</p>}<h3>참고할 소식</h3><ul>{result.fyi?.map((item, index) => <li key={index}>{item}</li>)}</ul><small>{result.volume_note} · {digest.data?.model}</small></article>}</div>
}
