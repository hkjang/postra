import {Link, useParams} from 'react-router-dom'
import {useQuery} from '@tanstack/react-query'
import {api} from '@/api/client'
import {Button, EmptyState, ErrorState, Loading, PageHeader, Panel} from '@/components/ui'
import {MailBody} from './MailBody'
import {addresses, mailDate} from './types'
import {messageViewResponse, responseList, responseObject} from './responses'

export function ThreadPage() {
  const {id = ''} = useParams()
  const thread = useQuery({queryKey: ['thread', id], queryFn: async ({signal}) => ({timeline: responseList(responseObject(await api<unknown>(`/api/threads/${encodeURIComponent(id)}/timeline`, {signal})).timeline, messageViewResponse)})})
  if (thread.isPending) return <Loading/>
  if (thread.error) return <ErrorState error={thread.error} retry={() => thread.refetch()}/>
  const messages = thread.data.timeline ?? []
  return <div className="page"><PageHeader title={messages[0]?.message.subject || '메일 대화'} description={`메일 ${messages.length}통 · 오래된 순서 · 내 메일만 표시합니다.`} actions={<Button variant="outline" asChild><Link to="/mail">받은메일로</Link></Button>}/>
    {!messages.length ? <EmptyState title="표시할 대화가 없습니다"/> : messages.map(({message, body}) => <Panel key={message.id}><div className="row between"><div><h2>{message.from.name || message.from.email}</h2><p className="small muted">{addresses(message.to)} · {mailDate(message.date)}</p></div><Button size="sm" variant="ghost" asChild><Link to={`/messages/${encodeURIComponent(message.id)}`}>단독 보기</Link></Button></div>{!!body?.external_images && !body.images_allowed && <p className="small muted">외부 이미지를 차단했습니다. 단독 보기에서 허용 정책을 확인할 수 있습니다.</p>}{body?.unavailable ? <ErrorState error={new Error(body.unavailable_reason || '본문을 불러올 수 없습니다.')}/> : <MailBody html={body?.html_sanitized} text={body?.text_body} receivedMessageID={message.id} title={`${message.subject || '메일'} 본문`}/>}</Panel>)}
  </div>
}
