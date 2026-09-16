import { useEffect, useRef, useState, type ReactNode } from 'react'
import { useMutation, useQueryClient } from '@tanstack/react-query'
import { useNavigate } from 'react-router-dom'
import { ArrowUpRight, CheckCheck, ListTodo, Sparkles } from 'lucide-react'
import { api } from '@/api/client'
import { Badge, Button, ErrorState, Textarea } from '@/components/ui'
import type { Analysis, Message } from './types'
import {actionCardsResponse, calendarResponse, createdDraftID, repliesResponse} from './responses'
import {usePersonalPreferences} from '@/features/settings/preferences'

type Summary = { summary?: string; requests?: string[]; dates?: string[] }
type Triage = { priority?: string; reply_required?: boolean; deadline?: string; recommended_next_action?: string; business_risks?: string[] }
function result(analysis: Analysis): Record<string, unknown> {
  try {
    const value: unknown = JSON.parse(analysis.result_json)
    if (!value || typeof value !== 'object' || Array.isArray(value)) throw new Error('Invalid AI result')
    return value as Record<string, unknown>
  } catch { throw new Error('AI 응답 형식을 읽을 수 없습니다. 다시 분석해 주세요.') }
}
function strings(value: unknown): string[] { return Array.isArray(value) ? value.filter((item): item is string => typeof item === 'string') : [] }
function summaryResult(analysis: Analysis): Summary {
  const value = result(analysis)
  if (typeof value.summary !== 'string') throw new Error('AI 응답에 요약이 없습니다. 다시 분석해 주세요.')
  return { summary: value.summary, requests: strings(value.requests), dates: strings(value.dates) }
}
function triageResult(analysis: Analysis): Triage {
  const value = result(analysis)
  return { priority: typeof value.priority === 'string' && ['urgent','high','normal','low'].includes(value.priority) ? value.priority : undefined, reply_required: typeof value.reply_required === 'boolean' ? value.reply_required : undefined, deadline: typeof value.deadline === 'string' ? value.deadline : undefined, recommended_next_action: typeof value.recommended_next_action === 'string' ? value.recommended_next_action : undefined, business_risks: strings(value.business_risks) }
}

export function AIContext({ message }: { message: Message }) {
  const navigate = useNavigate()
  const client = useQueryClient()
  const preferences = usePersonalPreferences()
  const autoSummary = preferences.value('ai.auto_summary','false') === 'true'
  const showReplies = preferences.value('ai.show_replies','true') === 'true'
  const autoStarted = useRef('')
  const summary = useMutation({ mutationFn: () => api<Analysis>(`/api/messages/${message.id}/analyze`, { method: 'POST', body: { type: 'summarize' } }).then(summaryResult) })
  const triage = useMutation({ mutationFn: () => api<Analysis>(`/api/messages/${message.id}/analyze`, { method: 'POST', body: { type: 'triage' } }).then(triageResult) })
  useEffect(() => {if (autoSummary && autoStarted.current !== message.id) {autoStarted.current=message.id; summary.mutate()}},[autoSummary,message.id])
  const replies = useMutation({ mutationFn: () => api<unknown>(`/api/messages/${message.id}/suggest-replies`, { method: 'POST', body: {} }).then(repliesResponse) })
  const cards = useMutation({ mutationFn: () => api<unknown>(`/api/messages/${message.id}/action-cards`, { method: 'POST', body: {} }).then(actionCardsResponse), onSuccess: () => client.invalidateQueries({ queryKey: ['action-cards'] }) })
  const reply = useMutation({
    mutationFn: (body: string) => api<unknown>('/api/drafts', { method: 'POST', body: { account_id: message.account_id, kind: 'reply', reply_to_message_id: message.id, body, body_author: 'ai' } }).then(createdDraftID),
    onSuccess: draftID => navigate(`/drafts/${encodeURIComponent(draftID)}`),
  })
  const [instructions, setInstructions] = useState('')
  const instructedReply = useMutation({mutationFn: () => api<unknown>('/api/drafts', {body: {account_id: message.account_id, kind: 'reply', reply_to_message_id: message.id, instructions}}).then(createdDraftID), onSuccess: draftID => navigate(`/drafts/${encodeURIComponent(draftID)}`)})
  return <aside className="ai-panel" aria-label="AI 컨텍스트">
    <div className="ai-panel-heading"><Sparkles size={18} /><h2>AI Insight</h2><Badge variant="secondary">검토 필요</Badge></div>
    <p className="muted">요약부터 답장까지, 이 메일의 다음 행동을 함께 정리합니다.</p>
    <section className="insight-section">
      <div className="row"><h3>핵심 요약</h3><Button size="sm" variant="ghost" disabled={summary.isPending} onClick={() => summary.mutate()}>{summary.isPending ? '분석 중…' : summary.data ? '다시 분석' : '요약하기'}</Button></div>
      {summary.error && <ErrorState error={summary.error} />}
      {summary.data ? <><p className="insight-prose">{summary.data.summary}</p>{(summary.data.requests ?? []).length > 0 && <ul>{summary.data.requests?.map((text, i) => <li key={i}>{text}</li>)}</ul>}{summary.data.dates?.map((date, i) => <Badge key={i} variant="outline">{date}</Badge>)}</> : <p className="muted">핵심 내용과 요청사항을 추려 보세요.</p>}
    </section>
    <section className="insight-section">
      <div className="row"><h3>중요도 · 다음 행동</h3><Button size="sm" variant="ghost" disabled={triage.isPending} onClick={() => triage.mutate()}>{triage.isPending ? '분석 중…' : '분석'}</Button></div>
      {triage.error && <ErrorState error={triage.error} />}
      {triage.data && <div className="stack"><div className="row"><Badge variant={['urgent', 'high'].includes(triage.data.priority ?? '') ? 'destructive' : 'secondary'}>{({ urgent: '긴급', high: '높음', normal: '보통', low: '낮음' } as Record<string, string>)[triage.data.priority ?? ''] ?? '미분류'}</Badge><Badge variant="outline">{triage.data.reply_required === undefined ? '답장 필요 여부 미확인' : triage.data.reply_required ? '답장 필요' : '답장 필수 아님'}</Badge></div>{triage.data.deadline && <p>기한: {triage.data.deadline}</p>}<p>{triage.data.recommended_next_action}</p>{triage.data.business_risks?.map((risk, i) => <p key={i} className="warning-text">{risk}</p>)}</div>}
    </section>
    <section className="insight-section">
      <div className="row"><h3><ListTodo size={16} /> 실행 항목</h3><Button size="sm" variant="ghost" disabled={cards.isPending || cards.isSuccess} onClick={() => cards.mutate()}>{cards.isPending ? '추출 중…' : cards.isSuccess ? '생성됨' : '카드 만들기'}</Button></div>
      {cards.error && <ErrorState error={cards.error} />}
      {cards.data && ((cards.data.cards ?? []).length ? <ul className="action-preview-list">{cards.data.cards.map(card => <li key={card.id}><CheckCheck size={15} /><div><strong>{card.title}</strong>{card.due && <small>{card.due}</small>}{card.confidence !== undefined && card.confidence < 0.5 && <small className="warning-text">낮은 신뢰도 · 직접 확인하세요</small>}</div></li>)}</ul> : <p className="muted">추출할 실행 항목이 없습니다.</p>)}
      {cards.isSuccess && <Button size="sm" variant="ghost" onClick={() => navigate('/actions')}>Action Center에서 검토 <ArrowUpRight size={14} /></Button>}
    </section>
    <section className="insight-section">
      <div className="row"><h3>스마트 답장</h3>{showReplies && <Button size="sm" variant="ghost" disabled={replies.isPending} onClick={() => replies.mutate()}>{replies.isPending ? '작성 중…' : '답장 제안'}</Button>}</div>
      {replies.error && <ErrorState error={replies.error} />}{reply.error && <ErrorState error={reply.error} />}
      {showReplies && replies.data && !replies.data.suggestions.length && <p className="muted">제안된 답장이 없습니다.</p>}
      {showReplies && replies.data?.suggestions.map((text, i) => <div className="reply-suggestion" key={i}><p>{text}</p><Button size="sm" variant="secondary" disabled={reply.isPending} onClick={() => reply.mutate(text)}>답장에 적용 <ArrowUpRight size={14} /></Button></div>)}
      {!showReplies && <p className="muted">개인 또는 조직 설정에서 추천 답장 표시를 껐습니다.</p>}
      <p className="muted">제안은 초안으로만 저장됩니다. 발송 전 확인과 승인이 필요합니다.</p>
      <form className="stack" onSubmit={event => {event.preventDefault(); instructedReply.mutate()}}><label className="field">AI 답장 작성 지시<Textarea value={instructions} onChange={event => setInstructions(event.target.value)} placeholder="예: 참석 가능하다고 정중하게 답장해 줘" required/></label><Button size="sm" type="submit" variant="outline" disabled={instructedReply.isPending || !instructions.trim()}>지시대로 답장 초안 만들기</Button>{instructedReply.error && <ErrorState error={instructedReply.error}/>}</form>
    </section>
    <AdditionalAnalysis messageID={message.id}/>
  </aside>
}

const fieldLabels: Record<string,string> = {action_items:'할 일',tasks:'할 일',items:'항목',task:'작업',title:'제목',description:'설명',owner:'담당',assignee:'담당자',due:'기한',deadline:'기한',category:'분류',importance:'중요도',reason:'이유',confidence:'신뢰도',people:'인물',organizations:'조직',amounts:'금액',dates:'날짜',entities:'추출 정보',name:'이름',value:'값',currency:'통화',risk:'위험도',risk_level:'위험도',is_phishing:'피싱 의심',signals:'판단 근거',indicators:'위험 징후',recommendation:'권고',recommended_action:'권장 조치',urls:'링크'}
function analysisContent(value: unknown, depth = 0): ReactNode {
  if (value == null) return '없음'
  if (depth > 6) return '세부 정보가 너무 깊습니다.'
  if (Array.isArray(value)) return value.length ? <ul>{value.slice(0,100).map((item,index) => <li key={index}>{analysisContent(item,depth+1)}</li>)}</ul> : '없음'
  if (typeof value === 'object') {
    const fields = Object.entries(value).filter(([key]) => !['__proto__','constructor','prototype'].includes(key)).slice(0,100)
    return fields.length ? <dl>{fields.map(([key,item]) => <div key={key}><dt><strong>{Object.hasOwn(fieldLabels,key) ? fieldLabels[key] : key}</strong></dt><dd style={{marginInlineStart: 12}}>{analysisContent(item,depth+1)}</dd></div>)}</dl> : '표시할 분석 항목이 없습니다.'
  }
  if (typeof value === 'boolean') return value ? '예' : '아니요'
  return String(value)
}
function AdditionalAnalysis({messageID}: {messageID: string}) {
  const [kind,setKind] = useState('action_items')
  const analyze = useMutation({mutationFn: () => api<Analysis>(`/api/messages/${encodeURIComponent(messageID)}/analyze`, {body: {type: kind}}).then(result)})
  const calendar = useMutation({mutationFn: () => api<unknown>(`/api/messages/${encodeURIComponent(messageID)}/calendar`, {method: 'POST'}).then(calendarResponse)})
  return <><section className="insight-section"><h3>상세 분석 · 보안 점검</h3><div className="stack"><select className="input" aria-label="상세 분석 종류" value={kind} onChange={event => {setKind(event.target.value); analyze.reset()}} disabled={analyze.isPending}>{[['action_items','할 일 추출'],['classify','분류 · 중요도'],['entities','인물 · 금액 추출'],['phishing','피싱 점검']].map(([value,label]) => <option key={value} value={value}>{label}</option>)}</select><Button variant="outline" size="sm" disabled={analyze.isPending} onClick={() => analyze.mutate()}>{analyze.isPending ? '분석 중…' : '선택한 분석 실행'}</Button>{analyze.error && <ErrorState error={analyze.error}/>}<div className="insight-prose">{analyze.data && analysisContent(analyze.data)}</div></div></section>
    <section className="insight-section"><h3>메일에서 일정 추출</h3><Button variant="outline" size="sm" disabled={calendar.isPending} onClick={() => calendar.mutate()}>{calendar.isPending ? '일정 확인 중…' : '일정 확인'}</Button>{calendar.error && <ErrorState error={calendar.error}/>} {calendar.data && <>{calendar.data.events?.length ? <><ul>{calendar.data.events.map((event,index) => <li key={index}><strong>{event.title}</strong><p>{event.start}{event.location ? ' · '+event.location : ''}</p></li>)}</ul><Button size="sm" variant="ghost" asChild><a href={`/api/messages/${encodeURIComponent(messageID)}/calendar.ics`} download>일정 파일 (.ics) 다운로드</a></Button></> : <p className="muted">추출할 일정이 없습니다.</p>}</>}</section></>
}
