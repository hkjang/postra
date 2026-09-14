import { useMutation, useQueryClient } from '@tanstack/react-query'
import { useNavigate } from 'react-router-dom'
import { ArrowUpRight, CheckCheck, ListTodo, Sparkles } from 'lucide-react'
import { api } from '@/api/client'
import { Badge, Button, ErrorState } from '@/components/ui'
import type { ActionCard, Analysis, DraftView, Message } from './types'

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
  return { priority: typeof value.priority === 'string' ? value.priority : undefined, reply_required: value.reply_required === true, deadline: typeof value.deadline === 'string' ? value.deadline : undefined, recommended_next_action: typeof value.recommended_next_action === 'string' ? value.recommended_next_action : undefined, business_risks: strings(value.business_risks) }
}

export function AIContext({ message }: { message: Message }) {
  const navigate = useNavigate()
  const client = useQueryClient()
  const summary = useMutation({ mutationFn: () => api<Analysis>(`/api/messages/${message.id}/analyze`, { method: 'POST', body: { type: 'summarize' } }).then(summaryResult) })
  const triage = useMutation({ mutationFn: () => api<Analysis>(`/api/messages/${message.id}/analyze`, { method: 'POST', body: { type: 'triage' } }).then(triageResult) })
  const replies = useMutation({ mutationFn: () => api<{ suggestions: string[] }>(`/api/messages/${message.id}/suggest-replies`, { method: 'POST', body: {} }) })
  const cards = useMutation({ mutationFn: () => api<{ cards: ActionCard[] }>(`/api/messages/${message.id}/action-cards`, { method: 'POST', body: {} }), onSuccess: () => client.invalidateQueries({ queryKey: ['action-cards'] }) })
  const reply = useMutation({
    mutationFn: (body: string) => api<DraftView>('/api/drafts', { method: 'POST', body: { account_id: message.account_id, kind: 'reply', reply_to_message_id: message.id, body, body_author: 'ai' } }),
    onSuccess: view => { client.setQueryData(['draft', view.draft.id], view); navigate(`/drafts/${view.draft.id}`) },
  })
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
      {triage.data && <div className="stack"><div className="row"><Badge variant={['urgent', 'high'].includes(triage.data.priority ?? '') ? 'destructive' : 'secondary'}>{({ urgent: '긴급', high: '높음', normal: '보통', low: '낮음' } as Record<string, string>)[triage.data.priority ?? ''] ?? '미분류'}</Badge><Badge variant="outline">{triage.data.reply_required ? '답장 필요' : '답장 필수 아님'}</Badge></div>{triage.data.deadline && <p>기한: {triage.data.deadline}</p>}<p>{triage.data.recommended_next_action}</p>{triage.data.business_risks?.map((risk, i) => <p key={i} className="warning-text">{risk}</p>)}</div>}
    </section>
    <section className="insight-section">
      <div className="row"><h3><ListTodo size={16} /> 실행 항목</h3><Button size="sm" variant="ghost" disabled={cards.isPending || cards.isSuccess} onClick={() => cards.mutate()}>{cards.isPending ? '추출 중…' : cards.isSuccess ? '생성됨' : '카드 만들기'}</Button></div>
      {cards.error && <ErrorState error={cards.error} />}
      {cards.data && ((cards.data.cards ?? []).length ? <ul className="action-preview-list">{cards.data.cards.map(card => <li key={card.id}><CheckCheck size={15} /><div><strong>{card.title}</strong>{card.due && <small>{card.due}</small>}{card.confidence !== undefined && card.confidence < 0.5 && <small className="warning-text">낮은 신뢰도 · 직접 확인하세요</small>}</div></li>)}</ul> : <p className="muted">추출할 실행 항목이 없습니다.</p>)}
      {cards.isSuccess && <Button size="sm" variant="ghost" onClick={() => navigate('/actions')}>Action Center에서 검토 <ArrowUpRight size={14} /></Button>}
    </section>
    <section className="insight-section">
      <div className="row"><h3>스마트 답장</h3><Button size="sm" variant="ghost" disabled={replies.isPending} onClick={() => replies.mutate()}>{replies.isPending ? '작성 중…' : '답장 제안'}</Button></div>
      {replies.error && <ErrorState error={replies.error} />}{reply.error && <ErrorState error={reply.error} />}
      {replies.data?.suggestions.map((text, i) => <div className="reply-suggestion" key={i}><p>{text}</p><Button size="sm" variant="secondary" disabled={reply.isPending} onClick={() => reply.mutate(text)}>답장에 적용 <ArrowUpRight size={14} /></Button></div>)}
      <p className="muted">제안은 초안으로만 저장됩니다. 발송 전 확인과 승인이 필요합니다.</p>
    </section>
  </aside>
}
