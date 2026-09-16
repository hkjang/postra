import {useState} from 'react'
import {useQuery} from '@tanstack/react-query'
import {Search} from 'lucide-react'
import {api} from '@/api/client'
import {Button, EmptyState, ErrorState, Input, Loading} from '@/components/ui'
import {messagePageResponse} from '../messages/responses'
import {mailDate, type Message} from '../messages/types'

// Search only the authenticated user's collected mail. No body, AI generation,
// read receipt, or action mutation is needed to choose a source message.
export function MessagePicker({onSelect, disabled}: {onSelect: (message: Message) => void; disabled: boolean}) {
  const [input, setInput] = useState('')
  const [query, setQuery] = useState('')
  const messages = useQuery({queryKey: ['action-message-picker', query], queryFn: ({signal}) => {
    const params = new URLSearchParams({limit: '10', q: query})
    return api<unknown>(`/api/messages?${params}`, {signal}).then(messagePageResponse)
  }})
  function search() {if (!disabled) setQuery(input.trim())}
  return <section className="action-message-picker stack" aria-label="액션 원본 메일 선택">
    <label className="field" htmlFor="action-mail-search">원본 메일 찾기</label>
    <div className="row"><Input id="action-mail-search" placeholder="메일 제목이나 내용으로 검색" value={input} disabled={disabled} maxLength={500} onChange={event => setInput(event.target.value)} onKeyDown={event => {if (event.key === 'Enter') {event.preventDefault(); search()}}}/><Button variant="outline" disabled={disabled} onClick={search}><Search size={16}/>메일 찾기</Button></div>
    <p className="small muted">{query ? '검색 결과' : '최근 메일'} 최대 10건 · 보관함과 다시 알림을 포함한 내 메일에서 선택합니다.</p>
    {messages.isPending ? <Loading label="원본 메일을 찾는 중…"/> : messages.error ? <ErrorState error={messages.error} retry={() => messages.refetch()}/> : !messages.data.messages.length ? <EmptyState title="선택할 메일이 없습니다" description="검색어를 바꾸거나 메일 동기화 상태를 확인하세요."/> : <ul className="action-source-results">{messages.data.messages.map(message => <li key={message.id}><button type="button" disabled={disabled} className="action-source-option" onClick={() => onSelect(message)} aria-label={`${message.subject || '제목 없는 메일'} · ${message.from.name || message.from.email} 선택`}><strong>{message.subject || '(제목 없음)'}</strong><span>{message.from.name || message.from.email}<time>{mailDate(message.date)}</time></span></button></li>)}</ul>}
  </section>
}
