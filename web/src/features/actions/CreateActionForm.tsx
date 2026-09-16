import {useContext, useEffect, useRef, useState, type FormEvent} from 'react'
import {Link, UNSAFE_DataRouterContext, useBlocker} from 'react-router-dom'
import {useMutation, useQuery, useQueryClient} from '@tanstack/react-query'
import * as Dialog from '@radix-ui/react-dialog'
import {toast} from 'sonner'
import {api, APIError} from '@/api/client'
import {InvalidResponseError} from '@/api/response'
import {Button, ErrorState, Input, Loading, Panel, Textarea} from '@/components/ui'
import {messageViewResponse} from '../messages/responses'
import {mailDate, type Message} from '../messages/types'
import {MessagePicker} from './MessagePicker'
import {parseActionCards} from './response'

function LeaveGuard({dirty, busy}: {dirty: boolean; busy: boolean}) {
  const blocker = useBlocker(({currentLocation, nextLocation}) => (dirty || busy) && currentLocation.pathname + currentLocation.search !== nextLocation.pathname + nextLocation.search)
  return <Dialog.Root open={blocker.state === 'blocked'} onOpenChange={open => {if (!open && blocker.state === 'blocked') blocker.reset()}}><Dialog.Portal><Dialog.Overlay className="dialog-overlay"/><Dialog.Content className="settings-confirm"><Dialog.Title>{busy ? '액션을 저장하고 있습니다' : '작성 중인 액션이 있습니다'}</Dialog.Title><Dialog.Description>{busy ? '저장이 끝난 뒤 이동해 주세요.' : '이동하면 저장하지 않은 액션 내용이 사라집니다.'}</Dialog.Description><div className="row"><Button variant="outline" onClick={() => {if (blocker.state === 'blocked') blocker.reset()}}>계속 작성</Button><Button disabled={busy} onClick={() => {if (!busy && blocker.state === 'blocked') blocker.proceed()}}>작성 취소하고 이동</Button></div></Dialog.Content></Dialog.Portal></Dialog.Root>
}

export function CreateActionForm({messageID: initialID, onClose}: {messageID: string; onClose: (showActions?: boolean) => void}) {
  const router = useContext(UNSAFE_DataRouterContext)
  const [source, setSource] = useState<Message>()
  const [title, setTitle] = useState('')
  const titleEdited = useRef(false)
  const sourceInitialized = useRef(false)
  const [detail, setDetail] = useState('')
  const [due, setDue] = useState('')
  const [assignee, setAssignee] = useState('')
  const [type, setType] = useState('todo')
  const [choosing, setChoosing] = useState(!initialID)
  const [logoutPending, setLogoutPending] = useState(false)
  const [uncertain, setUncertain] = useState(false)
  const logoutAllowed = useRef(false)
  const saving = useRef(false)
  const cache = useQueryClient()
  const initial = useQuery({queryKey: ['action-source', initialID], enabled: !!initialID, retry: false, queryFn: async ({signal}) => {
    const message = messageViewResponse(await api<unknown>(`/api/messages/${encodeURIComponent(initialID)}?body=false`, {signal})).message
    if (message.id !== initialID) throw new InvalidResponseError()
    return message
  }})
  function choose(message: Message) {
    if (logoutAllowed.current || saving.current) return
    sourceInitialized.current = true
    setSource(message); setChoosing(false)
    if (!titleEdited.current) setTitle(Array.from(message.subject || '').slice(0, 300).join(''))
  }
  useEffect(() => {if (initial.data && !sourceInitialized.current) choose(initial.data)}, [initial.data, logoutPending])
  const dirty = !!(source || title || detail || due || assignee || type !== 'todo')
  const create = useMutation({mutationFn: async () => {
    if (!source) throw new Error('원본 메일을 선택해 주세요.')
    const response = await api('/api/action-cards', {method: 'POST', body: {message_id: source.id, title: title.trim(), detail, due, assignee: assignee.trim(), type}})
    const card = parseActionCards({cards: [response]}).cards[0]
    if (!card.id || card.message_id !== source.id || card.status !== 'pending') throw new InvalidResponseError()
    return card
  }, onSuccess: () => {
    void cache.invalidateQueries({queryKey: ['action-cards']})
    toast.success('검토 대기 액션을 만들었습니다.'); onClose(true)
  }, onError: error => {
    // A lost/malformed success response can follow a committed write. Never
    // offer the same POST again as if it were a confirmed validation failure.
    if (!(error instanceof APIError && error.status >= 400 && error.status < 500 && error.status !== 408)) {
      setUncertain(true)
      void cache.invalidateQueries({queryKey: ['action-cards']})
    }
  }, onSettled: () => {saving.current = false}})
  const busy = create.isPending || logoutPending
  useEffect(() => {
    const leave = (event: BeforeUnloadEvent) => {if (!logoutAllowed.current && (dirty || saving.current)) {event.preventDefault(); event.returnValue = ''}}
    const logout = (event: Event) => {
      if (saving.current) {event.preventDefault(); toast.error('액션 저장이 끝난 뒤 로그아웃해 주세요.'); return}
      if (dirty && !window.confirm('작성 중인 액션을 버리고 로그아웃하시겠습니까?')) {event.preventDefault(); return}
      logoutAllowed.current = true; setLogoutPending(true)
    }
    const failed = () => {logoutAllowed.current = false; setLogoutPending(false)}
    window.addEventListener('beforeunload', leave); window.addEventListener('postra:before-logout', logout); window.addEventListener('postra:logout-failed', failed)
    return () => {window.removeEventListener('beforeunload', leave); window.removeEventListener('postra:before-logout', logout); window.removeEventListener('postra:logout-failed', failed)}
  }, [dirty])
  function submit(event: FormEvent) {
    event.preventDefault()
    if (saving.current || logoutAllowed.current || busy || uncertain || !source || !title.trim()) return
    saving.current = true; create.mutate()
  }
  function close() {if (!busy && !saving.current && (!dirty || window.confirm('작성 중인 액션을 버리시겠습니까?'))) onClose()}
  return <Panel className="action-create" aria-label="수동 액션 만들기">
    {router && <LeaveGuard dirty={dirty && !logoutPending} busy={create.isPending}/>}
    <form className="stack" onSubmit={submit}><h2>메일에서 할 일 만들기</h2><p className="muted">원본을 선택하고 할 일을 적으세요. 검토 대기로 저장되며 AI 호출·메일 발송·외부 등록은 하지 않습니다.</p>
      {source && <section className="action-selected-source" aria-label="선택한 원본 메일"><div><span className="eyebrow">연결된 메일</span><strong>{source.subject || '(제목 없음)'}</strong><p className="small muted">{source.from.name || source.from.email} · {mailDate(source.date)}</p><Link to={`/mail?message=${encodeURIComponent(source.id)}`}>원본 메일 보기 →</Link></div><Button variant="outline" size="sm" disabled={busy} onClick={() => setChoosing(value => !value)}>{choosing ? '선택 유지' : '다른 메일 선택'}</Button></section>}
      {!source && !!initialID && !choosing && <>{initial.isPending ? <Loading label="원본 메일 확인 중…"/> : initial.error ? <ErrorState error={initial.error} retry={() => initial.refetch()}/> : null}<Button variant="outline" disabled={busy} onClick={() => setChoosing(true)}>다른 메일 찾기</Button></>}
      {choosing && <MessagePicker onSelect={choose} disabled={busy}/>}
      <label className="field">액션 제목<Input required maxLength={300} disabled={busy} value={title} placeholder="예: 제안서 검토 후 회신" onChange={event => {titleEdited.current = true; setTitle(event.target.value)}}/></label>
      <div className="grid"><label className="field">유형<select className="input" disabled={busy} value={type} onChange={event => setType(event.target.value)}><option value="todo">할 일</option><option value="meeting">일정</option><option value="approval">승인</option><option value="inquiry">문의</option><option value="other">기타</option></select></label><label className="field">액션 기한<Input type="date" disabled={busy} value={due} onChange={event => setDue(event.target.value)}/></label></div>
      <label className="field">상세 내용<Textarea maxLength={10000} disabled={busy} value={detail} onChange={event => setDetail(event.target.value)}/></label><label className="field">액션 담당자<Input maxLength={128} disabled={busy} value={assignee} onChange={event => setAssignee(event.target.value)} placeholder="선택 사항"/></label>
      {create.error && <ErrorState error={create.error}/>}{uncertain && <div className="stack" role="status"><p>서버에 저장되었을 수 있어 중복 저장을 막았습니다. 작성 내용을 아래 목록과 비교하고, 없는 경우에만 새로 작성해 주세요.</p><Button variant="outline" disabled={busy} onClick={() => onClose(true)}>작성 닫고 액션 목록 확인</Button></div>}<div className="row action-form-controls"><Button type="submit" disabled={busy || uncertain || !source || !title.trim()}>{create.isPending ? '액션 저장 중…' : '액션 저장'}</Button><Button variant="ghost" disabled={busy} onClick={close}>닫기</Button><small className="muted">액션의 기한·담당자는 메일 업무함의 SLA·담당자와 별도로 저장됩니다.</small></div>
    </form>
  </Panel>
}
