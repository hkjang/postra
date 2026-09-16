import { useEffect, useRef, useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Link, useNavigate, useParams } from 'react-router-dom'
import { Archive, ArrowLeft, Download, Forward, Paperclip, Reply, ReplyAll, Star, Trash2 } from 'lucide-react'
import { api } from '@/api/client'
import { Badge, Button, EmptyState, ErrorState, Loading } from '@/components/ui'
import { AIContext } from './AIContext'
import { MailBody } from './MailBody'
import { ReceivedImages } from './ReceivedImages'
import { CopyMCPContext } from './CopyMCPContext'
import { registerMailCommands } from '@/lib/mail-commands'
import {usePersonalPreferences} from '@/features/settings/preferences'
import { addresses, mailDate } from './types'
import {batchResponse, createdDraftID, messageViewResponse} from './responses'
import '../inbox/mail.css'

export function MessagePane({ id, onClose }: { id: string; onClose?: () => void }) {
  const navigate = useNavigate()
  const client = useQueryClient()
  const preferences = usePersonalPreferences()
  const aiDefault = preferences.value('ui.ai_panel','true') === 'true'
  const [showAI, setShowAI] = useState(true)
  const [imageMessageID, setImageMessageID] = useState('')
  const [contextOpen, setContextOpen] = useState(false)
  const actionInFlight = useRef(false)
  const imagesOnce = imageMessageID === id
  useEffect(() => setShowAI(aiDefault),[aiDefault])
  const query = useQuery({ queryKey: ['message', id, imagesOnce], queryFn: ({ signal }) => api<unknown>(`/api/messages/${encodeURIComponent(id)}${imagesOnce ? '?external_images=once' : ''}`, { signal }).then(messageViewResponse) })
  const imageChoice = useMutation({
    mutationFn: ({scope,revoke}:{scope:'sender'|'domain';revoke:boolean}) => api<void>(`/api/messages/${encodeURIComponent(id)}/images/allow`,{method:revoke?'DELETE':'POST',body:{scope}}),
    onSuccess: () => { setImageMessageID(''); client.invalidateQueries({queryKey:['message',id]}); client.invalidateQueries({queryKey:['thread']}) },
  })
  const openedRead = useRef('')
  const reply = useMutation({
    mutationFn: (kind: 'reply' | 'reply_all' | 'forward') => api<unknown>('/api/drafts', { method: 'POST', body: { account_id: query.data?.message.account_id, kind, reply_to_message_id: id } }).then(createdDraftID),
    onSuccess: draftID => navigate(`/drafts/${encodeURIComponent(draftID)}`),
  })
  const update = useMutation({
    mutationFn: async (action: string) => {
      const value = batchResponse(await api<unknown>('/api/messages/batch', { method: 'POST', body: { message_ids: [id], action } }))
      if (value.failed) throw new Error(value.results.find(x => x.error)?.error || '메일 상태를 변경하지 못했습니다.')
      return value
    },
    onSuccess: (_, action) => { client.invalidateQueries({ queryKey: ['messages'] }); client.invalidateQueries({ queryKey: ['message', id] }); if (action === 'delete') {if (onClose) onClose(); else navigate('/mail')} },
  })
  useEffect(()=>{if(query.data?.message.is_read===false && openedRead.current!==id){openedRead.current=id;update.mutate('mark_read')}},[id,query.data?.message.is_read])
  function replyTo(kind: 'reply' | 'reply_all' | 'forward') {
    if (!query.data || query.error || reply.isPending || update.isPending || actionInFlight.current) return
    actionInFlight.current = true
    reply.mutate(kind, { onSettled: () => { actionInFlight.current = false } })
  }
  function archive() {
    if (!query.data || query.error || query.data.message.is_archived || reply.isPending || update.isPending || actionInFlight.current) return
    actionInFlight.current = true
    update.mutate('archive', { onSettled: () => { actionInFlight.current = false } })
  }
  useEffect(() => {
    if (!query.data || query.error || query.data.message.id !== id) return
    return registerMailCommands({
      copy_context: () => setContextOpen(true),
      ...(!reply.isPending && !update.isPending ? { reply: () => replyTo('reply'), reply_all: () => replyTo('reply_all'), forward: () => replyTo('forward'), ...(!query.data.message.is_archived ? { archive } : {}), ...(onClose ? { close: onClose } : {}) } : {}),
    })
  }, [id, query.data, query.error, reply.isPending, update.isPending, onClose])
  if (query.isPending) return <div className="message-pane"><Loading /></div>
  if (query.error) return <div className="message-pane"><ErrorState error={query.error} retry={() => query.refetch()} /></div>
  const { message: m, body, attachments } = query.data
  return <div className={`message-detail-layout ${showAI ? '' : 'without-ai'}`}>
    <article className="message-pane">
      <div className="message-toolbar">
        {onClose && <Button size="icon" variant="ghost" onClick={onClose} aria-label="목록으로 돌아가기"><ArrowLeft size={18} /></Button>}
        <Button size="icon" variant="ghost" aria-label={m.is_important ? '중요 표시 해제' : '중요 메일 표시'} disabled={update.isPending} onClick={() => update.mutate(m.is_important ? 'unmark_important' : 'mark_important')}><Star size={18} fill={m.is_important ? 'currentColor' : 'none'} /></Button>
        <Button size="sm" variant="ghost" disabled={update.isPending} onClick={() => update.mutate(m.is_archived ? 'unarchive' : 'archive')}><Archive size={17} />{m.is_archived ? '보관 해제' : '보관'}</Button>
        <Button size="icon" variant="ghost" disabled={update.isPending} aria-label="내 메일 데이터 삭제" onClick={() => {if (window.confirm('이 메일의 Postra 데이터를 삭제하시겠습니까? 메일 서버의 원본은 그대로 유지됩니다.')) update.mutate('delete')}}><Trash2 size={17}/></Button>
        <Button size="sm" variant="ghost" disabled={update.isPending} onClick={()=>update.mutate(m.is_read?'mark_unread':'mark_read')}>{m.is_read?'안읽음 표시':'읽음 표시'}</Button>
        <Button size="sm" variant="ghost" disabled={preferences.locked('ui.ai_panel')} onClick={() => setShowAI(x => !x)} aria-pressed={showAI}>AI 패널 {showAI ? '접기' : '열기'}</Button>
        <CopyMCPContext key={id} message={m} open={contextOpen} onOpenChange={setContextOpen}/>
      </div>
      {update.error && <ErrorState error={update.error} />}{reply.error && <ErrorState error={reply.error} />}
      {m.thread_id && <Button variant="ghost" size="sm" asChild><Link to={`/threads/${encodeURIComponent(m.thread_id)}`}>대화 전체 보기</Link></Button>}
      {m.auth_results&&<p className="message-attachments small muted">메일 인증: {m.auth_results}</p>}{m.parse_error&&<ErrorState error={new Error('부분 파싱: '+m.parse_error)}/>}
      <header className="message-header"><h1>{m.subject || '(제목 없음)'}</h1><div className="sender-line"><span className="sender-avatar">{(m.from.name || m.from.email).charAt(0).toUpperCase()}</span><div><strong>{m.from.name || m.from.email}</strong><p className="muted">{m.from.email}</p></div><time>{mailDate(m.date)}</time></div><dl className="recipient-details"><dt>받는 사람</dt><dd>{addresses(m.to)}</dd>{!!m.cc?.length && <><dt>참조</dt><dd>{addresses(m.cc)}</dd></>}</dl><div className="row">{m.labels?.map(label => <Badge key={label} variant="outline">{label}</Badge>)}</div></header>
      <ReceivedImages body={body} busy={query.isFetching || imageChoice.isPending} error={imageChoice.error} once={()=>setImageMessageID(id)} trust={(scope,revoke)=>imageChoice.mutate({scope,revoke})}/>
      {body?.unavailable ? <ErrorState error={new Error(body.unavailable_reason || '본문을 불러올 수 없습니다. 계정에서 본문을 재동기화해 주세요.')} /> : <MailBody key={`${id}:${body?.images_allowed}:${body?.image_sender_trusted}:${body?.image_domain_trusted}`} html={body?.html_sanitized} text={body?.text_body} receivedMessageID={id} allowImagesOnce={imagesOnce} />}
      {body?.html_sanitized && body.text_body && <details className="message-attachments"><summary>원문 텍스트 보기</summary><div className="pre-wrap">{body.text_body}</div></details>}
      {!!attachments?.length && <section className="message-attachments"><h3><Paperclip size={16} /> 첨부파일 {attachments.length}</h3>{attachments.map(att => <div className="attachment-row" key={att.id}><div><strong>{att.name}</strong><small>{Math.ceil(att.size / 1024)} KB · {att.scan_status}</small>{att.scan_detail && <small>{att.scan_detail}</small>}</div>{att.scan_status === 'blocked' ? <span className="danger-text">보안 정책으로 차단됨</span> : <a className="attachment-download" href={`/api/messages/${encodeURIComponent(id)}/attachments/${encodeURIComponent(att.id)}${att.scan_status === 'clean' ? '' : '?ack=true'}`} download onClick={event => {if (att.scan_status !== 'clean' && !window.confirm('격리 또는 의심 첨부입니다. 위험을 이해하고 다운로드하시겠습니까?')) event.preventDefault()}}><Download size={16}/><span>{att.scan_status === 'clean' ? '다운로드' : '위험 확인 후 다운로드'}</span></a>}</div>)}</section>}
      <div className="message-attachments"><Button variant="ghost" size="sm" asChild><Link to={`/team?message=${encodeURIComponent(id)}`}>담당자 · 처리 상태 · 메모 관리</Link></Button></div>
      <footer className="message-actions"><Button disabled={reply.isPending || update.isPending} onClick={() => replyTo('reply')} title="답장 (R)"><Reply size={17} /> 답장</Button><Button variant="secondary" disabled={reply.isPending || update.isPending} onClick={() => replyTo('reply_all')} title="전체 답장 (A)"><ReplyAll size={17} /> 전체 답장</Button><Button variant="ghost" disabled={reply.isPending || update.isPending} onClick={() => replyTo('forward')} title="전달 (F)"><Forward size={17} /> 전달</Button></footer>
    </article>
    {showAI && <AIContext key={id} message={m} />}
  </div>
}

export function MessagePage() {
  const { id } = useParams()
  const navigate = useNavigate()
  return id ? <MessagePane key={id} id={id} onClose={() => navigate('/mail')} /> : <EmptyState title="메일을 선택하세요" description="받은메일에서 확인할 메일을 선택해 주세요." />
}
