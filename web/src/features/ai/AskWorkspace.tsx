import {useState, type FormEvent} from 'react'
import {Link, useSearchParams} from 'react-router-dom'
import {useMutation, useQuery} from '@tanstack/react-query'
import {ArrowUp, FileText, Sparkles} from 'lucide-react'
import {api} from '@/api/client'
import {Button, ErrorState, Input, Loading, Textarea} from '@/components/ui'
import type {Account} from '@/features/messages/types'
import type {AskInput, AskResult} from '@/api/contracts.generated'
import {usePersonalPreferences} from '@/features/settings/preferences'
import {formatDate} from '@/lib/utils'
import {resultOf} from './index'

export type AskResponse = AskResult

export function AskWorkspace() {
  const [params] = useSearchParams()
  const [question, setQuestion] = useState(params.get('q') || '')
  const [account, setAccount] = useState('')
  const [mode, setMode] = useState('auto')
  const [searchText, setSearchText] = useState('')
  const [from, setFrom] = useState('')
  const [since, setSince] = useState('')
  const [until, setUntil] = useState('')
  const [incomplete, setIncomplete] = useState(false)
  const accounts = useQuery({queryKey: ['accounts'], queryFn: () => api<Account[]>('/api/accounts')})
  const preferences = usePersonalPreferences()
  const lockedMode = preferences.locked('search.default_mode')
  const effectiveMode = lockedMode ? preferences.value('search.default_mode', 'keyword') : mode
  const answer = useMutation({mutationFn: () => {
    const start = since ? new Date(`${since}T00:00:00`) : undefined
    const end = until ? new Date(`${until}T00:00:00`) : undefined
    end?.setDate(end.getDate() + 1)
    return api<AskResponse>('/api/qa', {body: {question, account_id: account, mode: effectiveMode,
      search_text: searchText, from, since: start ? Math.floor(start.getTime() / 1000) : 0,
      until: end ? Math.floor(end.getTime() / 1000) : 0,
      time_zone: Intl.DateTimeFormat().resolvedOptions().timeZone || 'UTC', incomplete_only: incomplete} satisfies AskInput})
  }})
  const result = resultOf(answer.data?.result_json === undefined ? undefined : {id: answer.data.id || '', model: answer.data.model || '', result_json: answer.data.result_json})
  const retrieval = answer.data?.retrieval
  const cited = new Set(result?.evidence_message_ids || [])
  function ask(event: FormEvent) {event.preventDefault(); if (question.trim()) answer.mutate()}
  return <div className="page ai-page">
    <div className="ai-hero"><Sparkles size={32}/><h1>Ask Postra</h1><p className="muted">내 메일, 업무 상태와 Action을 근거로 답변합니다.</p></div>
    <form className="stack" onSubmit={ask}>
      <div className="ai-question"><Textarea aria-label="AI에게 질문" value={question} maxLength={4000} onChange={event => setQuestion(event.target.value)} placeholder="지난주 자료 요청과 관련된 미완료 업무를 정리해줘" required/><Button type="submit" disabled={answer.isPending || !question.trim()}><ArrowUp size={17}/>{answer.isPending ? '근거를 찾는 중…' : '질문하기'}</Button></div>
      {accounts.error && <ErrorState error={accounts.error}/>}<div className="row filters">
        <label>대상 계정<select className="input" value={account} onChange={event => setAccount(event.target.value)}><option value="">내 모든 계정</option>{accounts.data?.map(item => <option key={item.id} value={item.id}>{item.email}</option>)}</select></label>
        <label>검색 방식<select className="input" value={effectiveMode} disabled={lockedMode} onChange={event => setMode(event.target.value)}><option value="auto">자동 · 개인 검색 기본값</option><option value="keyword">키워드</option><option value="semantic">의미</option><option value="hybrid">통합</option><option value="work">업무</option><option value="actions">Action</option></select></label>
        <label className="row"><input type="checkbox" checked={incomplete} onChange={event => setIncomplete(event.target.checked)}/>완료된 업무 제외</label>
      </div>
      <details><summary>검색 범위 지정</summary><div className="form-grid">
        <label>검색어<Input value={searchText} onChange={event => setSearchText(event.target.value)} placeholder="예: 견적서 (지정하면 대체 검색하지 않음)"/></label>
        <label>발신자<Input value={from} onChange={event => setFrom(event.target.value)} placeholder="이메일 또는 이름"/></label>
        <label>시작일<Input type="date" value={since} onChange={event => setSince(event.target.value)}/></label>
        <label>종료일<Input type="date" min={since || undefined} value={until} onChange={event => setUntil(event.target.value)}/></label>
      </div><p className="small muted">기간은 발신 메일 날짜 기준입니다. 명시한 기간이 질문의 상대 날짜보다 우선합니다. 업무 검색의 검색어는 제목·담당자, Action 검색은 제목·설명에서 찾습니다.</p></details>
    </form>
    {answer.isPending && <Loading label="내 메일과 연결된 업무에서 근거를 찾고 있습니다…"/>}{answer.error && <ErrorState error={answer.error}/>}
    <p className="small muted">실행 시 설정된 AI가 접근 가능한 근거를 분석합니다. 최대 6개 메일을 참고하며 전체 메일을 조사한 결과가 아닙니다. 업무 상태만으로 미회신 여부를 단정하지 않습니다.</p>
    {result && <article className="ai-result"><h2><Sparkles size={18}/>근거를 바탕으로 한 답변</h2>
      {retrieval?.warnings?.map((warning, index) => <p className="warning-banner" role="note" key={index}>{warning}</p>)}
      <p className="pre-wrap">{result.answer || '답변을 생성하지 못했습니다.'}</p>
      {retrieval ? <><h3>참고한 근거 {retrieval.sources?.length || 0}개</h3><p className="small muted">{retrieval.mode} · {retrieval.time_zone}{retrieval.since ? ` · ${formatDate(retrieval.since)}부터` : ''}{retrieval.until ? ` · ${formatDate(retrieval.until)} 이전` : ''}</p><div className="stack">{retrieval.sources?.map(source => <Link key={source.message_id} to={`/messages/${encodeURIComponent(source.message_id)}`}><FileText size={14}/> {source.subject || '(제목 없음)'}<span className="small muted"> · {source.from}{cited.has(source.message_id) ? ' · AI 인용' : ''}{source.work_status ? ` · ${source.work_status}` : ''}{source.action_ids?.length ? ` · Action ${source.action_ids.length}` : ''}</span></Link>)}</div></> : !!result.evidence_message_ids?.length && <div className="evidence-list">{result.evidence_message_ids.map((id, index) => <Link key={id} to={`/messages/${encodeURIComponent(id)}`}><FileText size={12}/>근거 {index + 1}</Link>)}</div>}
      <p className="small muted">{answer.data?.model}{typeof result.confidence === 'number' ? ` · 모델이 표시한 신뢰도 ${Math.round(result.confidence * 100)}%` : ''}</p>
    </article>}
  </div>
}
