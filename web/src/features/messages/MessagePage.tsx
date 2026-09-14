import { useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useNavigate, useParams } from 'react-router-dom'
import { Archive, ArrowLeft, Download, Forward, Paperclip, Reply, ReplyAll, Star } from 'lucide-react'
import { api } from '@/api/client'
import { Badge, Button, EmptyState, ErrorState, Loading } from '@/components/ui'
import { AIContext } from './AIContext'
import { MailBody } from './MailBody'
import { addresses, mailDate, type DraftView, type MessageView } from './types'
import '../inbox/mail.css'

export function MessagePane({ id, onClose }: { id: string; onClose?: () => void }) {
  const navigate = useNavigate()
  const client = useQueryClient()
  const [showAI, setShowAI] = useState(true)
  const query = useQuery({ queryKey: ['message', id], queryFn: ({ signal }) => api<MessageView>(`/api/messages/${encodeURIComponent(id)}`, { signal }) })
  const reply = useMutation({
    mutationFn: (kind: 'reply' | 'reply_all' | 'forward') => api<DraftView>('/api/drafts', { method: 'POST', body: { account_id: query.data?.message.account_id, kind, reply_to_message_id: id } }),
    onSuccess: view => { client.setQueryData(['draft', view.draft.id], view); navigate(`/drafts/${view.draft.id}`) },
  })
  const update = useMutation({
    mutationFn: async (action: string) => {
      const value = await api<{ failed: number; results: { error?: string }[] }>('/api/messages/batch', { method: 'POST', body: { message_ids: [id], action } })
      if (value.failed) throw new Error(value.results.find(x => x.error)?.error || '메일 상태를 변경하지 못했습니다.')
      return value
    },
    onSuccess: () => { client.invalidateQueries({ queryKey: ['messages'] }); client.invalidateQueries({ queryKey: ['message', id] }) },
  })
  if (query.isPending) return <div className="message-pane"><Loading /></div>
  if (query.error) return <div className="message-pane"><ErrorState error={query.error} retry={() => query.refetch()} /></div>
  const { message: m, body, attachments } = query.data
  return <div className={`message-detail-layout ${showAI ? '' : 'without-ai'}`}>
    <article className="message-pane">
      <div className="message-toolbar">
        {onClose && <Button size="icon" variant="ghost" onClick={onClose} aria-label="목록으로 돌아가기"><ArrowLeft size={18} /></Button>}
        <Button size="icon" variant="ghost" aria-label={m.is_important ? '중요 표시 해제' : '중요 메일 표시'} disabled={update.isPending} onClick={() => update.mutate(m.is_important ? 'unmark_important' : 'mark_important')}><Star size={18} fill={m.is_important ? 'currentColor' : 'none'} /></Button>
        <Button size="sm" variant="ghost" disabled={update.isPending} onClick={() => update.mutate(m.is_archived ? 'unarchive' : 'archive')}><Archive size={17} />{m.is_archived ? '보관 해제' : '보관'}</Button>
        <Button size="sm" variant="ghost" onClick={() => setShowAI(x => !x)} aria-pressed={showAI}>AI 패널 {showAI ? '접기' : '열기'}</Button>
      </div>
      {update.error && <ErrorState error={update.error} />}{reply.error && <ErrorState error={reply.error} />}
      <header className="message-header"><h1>{m.subject || '(제목 없음)'}</h1><div className="sender-line"><span className="sender-avatar">{(m.from.name || m.from.email).charAt(0).toUpperCase()}</span><div><strong>{m.from.name || m.from.email}</strong><p className="muted">{m.from.email}</p></div><time>{mailDate(m.date)}</time></div><dl className="recipient-details"><dt>받는 사람</dt><dd>{addresses(m.to)}</dd>{!!m.cc?.length && <><dt>참조</dt><dd>{addresses(m.cc)}</dd></>}</dl><div className="row">{m.labels?.map(label => <Badge key={label} variant="outline">{label}</Badge>)}</div></header>
      {body?.unavailable ? <ErrorState error={new Error(body.unavailable_reason || '본문을 불러올 수 없습니다. 계정에서 본문을 재동기화해 주세요.')} /> : <MailBody html={body?.html_sanitized} text={body?.text_body} />}
      {!!attachments?.length && <section className="message-attachments"><h3><Paperclip size={16} /> 첨부파일 {attachments.length}</h3>{attachments.map(att => <div className="attachment-row" key={att.id}><div><strong>{att.name}</strong><small>{Math.ceil(att.size / 1024)} KB · {att.scan_status}</small></div>{att.scan_status === 'clean' ? <a className="attachment-download" href={`/api/messages/${encodeURIComponent(id)}/attachments/${encodeURIComponent(att.id)}`} download><Download size={16} /><span>다운로드</span></a> : <span className="warning-text">보안 확인 필요 · <a href={`/ui/messages/${encodeURIComponent(id)}`}>상세 확인</a></span>}</div>)}</section>}
      <footer className="message-actions"><Button disabled={reply.isPending} onClick={() => reply.mutate('reply')}><Reply size={17} /> 답장</Button><Button variant="secondary" disabled={reply.isPending} onClick={() => reply.mutate('reply_all')}><ReplyAll size={17} /> 전체 답장</Button><Button variant="ghost" disabled={reply.isPending} onClick={() => reply.mutate('forward')}><Forward size={17} /> 전달</Button></footer>
    </article>
    {showAI && <AIContext key={id} message={m} />}
  </div>
}

export function MessagePage() {
  const { id } = useParams()
  const navigate = useNavigate()
  return id ? <MessagePane key={id} id={id} onClose={() => navigate('/mail')} /> : <EmptyState title="메일을 선택하세요" description="받은메일에서 확인할 메일을 선택해 주세요." />
}
