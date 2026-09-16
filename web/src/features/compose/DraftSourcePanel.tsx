import {useId, useState} from 'react'
import {useQuery} from '@tanstack/react-query'
import {Link} from 'react-router-dom'
import {ChevronDown, ChevronUp, ExternalLink} from 'lucide-react'
import {api} from '@/api/client'
import {InvalidResponseError} from '@/api/response'
import {Button, ErrorState, Loading} from '@/components/ui'
import {MailBody} from '@/features/messages/MailBody'
import {MailText} from '@/features/messages/MailText'
import {messageViewResponse} from '@/features/messages/responses'
import {addresses, mailDate} from '@/features/messages/types'
import './draft-source.css'

type Props = {draftID: string; kind?: string; messageID?: string}

// A source preview is deliberately not MessagePane: opening it must not mark
// mail read, start AI analysis, insert text, alter approval or load remote images.
export function DraftSourcePanel(props: Props) {
  if (!['reply', 'reply_all', 'forward'].includes(props.kind || '')) return null
  const referenceKey = typeof props.messageID === 'string' ? props.messageID : ''
  return <SourceReference key={`${props.draftID}:${referenceKey}`} {...props}/>
}

function SourceReference({draftID, kind, messageID}: Props) {
  const [open, setOpen] = useState(false)
  const headingID = useId(), contentID = useId()
  const validReference = typeof messageID === 'string' && messageID.trim().length > 0 && messageID.length <= 1024 && !/[\x00-\x1f\x7f]/.test(messageID)
  const source = useQuery({
    queryKey: ['draft-source', draftID, messageID],
    enabled: open && validReference,
    retry: false,
    staleTime: 60_000,
    refetchOnWindowFocus: false,
    queryFn: async ({signal}) => {
      const view = messageViewResponse(await api<unknown>(`/api/messages/${encodeURIComponent(messageID!)}`, {signal}))
      if (view.message.id !== messageID) throw new InvalidResponseError()
      return view
    },
  })
  const view = source.data
  const title = kind === 'forward' ? '전달 원문' : kind === 'reply_all' ? '전체 답장 원문' : '답장 원문'
  return <section className="draft-source" aria-labelledby={headingID}>
    <div className="draft-source-heading"><div><h2 id={headingID}>{title}</h2><p className="muted">작성 중 원문을 확인하세요. 펼쳐도 작성 내용과 메일 읽음 상태는 바뀌지 않습니다.</p></div><Button variant="outline" size="sm" disabled={!validReference} aria-expanded={open} aria-controls={contentID} onClick={() => setOpen(value => !value)}>{open ? <ChevronUp size={16}/> : <ChevronDown size={16}/>}{open ? '원문 접기' : '원문 펼치기'}</Button></div>
    {!validReference && <ErrorState error={new Error('원본 메일 참조를 확인할 수 없습니다. 초안은 계속 작성할 수 있습니다.')}/>}
    {open && <div id={contentID} className="draft-source-content">
      {source.isPending ? <Loading label="원본 메일을 확인하는 중…"/> : source.error ? <ErrorState error={new Error('원본 메일을 불러오지 못했습니다. 삭제되었거나 접근 권한이 없을 수 있습니다. 초안은 계속 작성할 수 있습니다.')} retry={() => {void source.refetch()}}/> : view && <>
        <div className="draft-source-envelope"><h3>{view.message.subject || '(제목 없음)'}</h3><dl><dt>보낸 사람</dt><dd>{addresses([view.message.from])}</dd><dt>받는 사람</dt><dd>{addresses(view.message.to) || '정보 없음'}</dd>{!!view.message.cc?.length && <><dt>참조</dt><dd>{addresses(view.message.cc)}</dd></>}<dt>보낸 시각</dt><dd>{mailDate(view.message.date)}</dd></dl><div className="row"><Link to={`/messages/${encodeURIComponent(messageID!)}`} target="_blank" rel="noopener noreferrer">원본 메일 새 탭에서 열기 <ExternalLink size={13}/></Link>{!!view.attachments?.length && <span className="muted">원본 첨부 {view.attachments.length}개 · 이 초안에 자동 추가하지 않습니다.</span>}</div></div>
        <p className="draft-source-notice muted">읽기 전용 미리보기입니다. 외부 이미지는 불러오지 않으며 원문 내용은 자동으로 본문에 삽입하지 않습니다. HTML 본문의 링크나 첨부파일을 확인하려면 위의 ‘원본 메일 새 탭에서 열기’를 사용하세요.</p>
        {view.body?.unavailable ? <p className="draft-source-notice" role="status">원본 본문을 사용할 수 없습니다. 계정의 동기화 상태를 확인해 주세요.</p> : view.body?.text_body.trim() ? <MailText text={view.body.text_body}/> : view.body?.html_sanitized ? <MailBody html={view.body.html_sanitized} title="원본 메일 서식 미리보기"/> : <p className="draft-source-notice muted">표시할 원본 본문이 없습니다.</p>}
      </>}
    </div>}
  </section>
}
