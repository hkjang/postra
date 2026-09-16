import { useEffect, useRef, useState, type FormEvent } from 'react'
import { useInfiniteQuery } from '@tanstack/react-query'
import { useNavigate, useParams, useSearchParams } from 'react-router-dom'
import { Virtuoso, type VirtuosoHandle } from 'react-virtuoso'
import { ArrowDown, Inbox, ListFilter, Paperclip, RefreshCw, Search, Star } from 'lucide-react'
import { api } from '@/api/client'
import { Badge, Button, EmptyState, ErrorState, Input, Loading } from '@/components/ui'
import { MessagePane } from '../messages/MessagePage'
import { mailDate, type Message, type MessageView } from '../messages/types'
import {AdvancedSearch, BulkActions, keywordSearchParams} from './SearchTools'
import {usePreferenceBridge} from '@/features/settings/usePreferenceBridge'
import {registerMailCommands} from '@/lib/mail-commands'
import {messagePageResponse, messageViewResponse, responseList, responseObject} from '../messages/responses'
import './mail.css'
import './search-tools.css'

type MailPage = { messages: Message[]; next_cursor?: string; snippets?:Record<string,string>; views?: Record<string, MessageView> }
type SearchMode = 'keyword' | 'semantic' | 'hybrid'
const filters = [{ id: 'inbox', label: '전체' },{id:'unread',label:'안읽음'}, { id: 'important', label: '중요' }, { id: 'attachment', label: '첨부' }, { id: 'today', label: '오늘' }, { id: 'archive', label: '보관함' }, {id: 'snoozed', label: '다시 알림'}]

export function mailRefreshInterval(query: string, mode: SearchMode, folder: string): number | false {
  // A mode without a search term still uses GET /messages. Never poll the
  // embedding-backed semantic/hybrid POST search just to refresh reminders.
  return (!query || mode === 'keyword') && ['inbox', 'snoozed'].includes(folder) ? 60_000 : false
}

export function InboxPage() {
  const preferences = usePreferenceBridge()
  const [params, setParams] = useSearchParams()
  const { id } = useParams()
  const navigate = useNavigate()
  const selected = id || params.get('message') || ''
  const query = params.get('q') ?? ''
  const preferredMode = preferences.value('search.default_mode','keyword')
  const selectedMode = preferences.locked('search.default_mode') ? preferredMode : params.get('mode') || preferredMode
  const mode = (['semantic', 'hybrid'].includes(selectedMode) ? selectedMode : 'keyword') as SearchMode
  const reader = preferences.value('ui.reader_position','right')
  const previewLines = Math.max(0,Math.min(5,Number(preferences.value('ui.preview_lines','2')) || 0))
  const folder = params.get('folder') || 'inbox'
  const account = params.get('account') || ''
  const [input, setInput] = useState(query)
  const compact = preferences.value('ui.density', 'comfortable') === 'compact'
  const [checked, setChecked] = useState<Set<string>>(new Set())
  const list = useRef<VirtuosoHandle>(null)
  const filterKey = keywordSearchParams(params, '').toString()
  useEffect(() => setChecked(new Set()), [filterKey, mode])
  useEffect(() => setInput(query), [query])
  function change(values: Record<string, string>) {
    const next = new URLSearchParams(params)
    for (const [key, value] of Object.entries(values)) { if (value) next.set(key, value); else next.delete(key) }
    if (id) navigate(`/mail?${next}`)
    else setParams(next)
  }
  const mail = useInfiniteQuery({
    queryKey: ['messages', { query, mode, folder, account, filterKey }],
    refetchInterval: mailRefreshInterval(query, mode, folder),
    refetchIntervalInBackground: false,
    initialPageParam: '',
    queryFn: async ({ pageParam, signal }): Promise<MailPage> => {
      if (query && mode !== 'keyword') {
        const body = mode === 'hybrid' ? { Query: query, AccountID: account, Limit: 100 } : { query, account_id: account, limit: 100 }
        const result = responseObject(await api<unknown>(`/api/${mode}-search`, { method: 'POST', body, signal }))
        const views = responseList(result.results, messageViewResponse)
        return { messages: views.map(view => view.message), views: Object.fromEntries(views.map(view => [view.message.id, view])) }
      }
      const search = keywordSearchParams(params, pageParam)
      return api<unknown>(`/api/messages?${search}`, { signal }).then(messagePageResponse)
    },
    getNextPageParam: page => page.next_cursor || undefined,
  })
  const messages = mail.data?.pages.flatMap(page => page.messages ?? []) ?? []
  const views = Object.assign({}, ...mail.data?.pages.map(page => page.views ?? {}) ?? []) as Record<string, MessageView>
  const snippets = Object.assign({}, ...mail.data?.pages.map(page => page.snippets ?? {}) ?? []) as Record<string,string>
  function search(event: FormEvent) { event.preventDefault(); change({ q: input.trim(), message: '' }) }
  function select(message: Message) { if (reader === 'hidden') navigate(`/messages/${encodeURIComponent(message.id)}`); else change({ message: message.id }) }
  useEffect(() => {
    if (mail.error || !messages.length) return
    const current = messages.findIndex(message => message.id === selected)
    function move(index: number) { select(messages[index]); list.current?.scrollIntoView({ index, behavior: 'auto' }) }
    return registerMailCommands({
      ...(current < messages.length - 1 ? { next: () => move(current + 1) } : {}),
      ...(current > 0 ? { previous: () => move(current - 1) } : current < 0 ? { previous: () => move(0) } : {}),
    })
  }, [mail.data, mail.error, selected, reader, params])
  return <div className={`split-workspace mail-workspace reader-${reader} ${selected && reader !== 'hidden' ? 'has-selection' : ''}`}>
    <section className={`mail-list ${compact ? 'compact' : ''}`} aria-label="메일 목록">
      <header className="mail-list-header"><div className="row"><div><p className="eyebrow">YOUR WORKSPACE</p><h1>{query ? '검색 결과' : folder === 'important' ? '중요 메일' : folder === 'archive' ? '보관함' : '받은메일'}</h1></div><Button size="icon" variant="ghost" aria-label="메일 목록 새로고침" disabled={mail.isFetching} onClick={() => mail.refetch()}><RefreshCw size={18} className={mail.isFetching ? 'spin' : ''} /></Button></div>
        <form className="mail-search" onSubmit={search}><Search size={17} /><Input aria-label="메일 검색어" placeholder="메일에서 검색…" value={input} onChange={event => setInput(event.target.value)} /><Button variant="ghost" size="sm" type="submit">검색</Button></form>
        <div className="row mail-search-options"><select aria-label="검색 방식" value={mode} disabled={preferences.locked('search.default_mode')} onChange={event => change({ mode: event.target.value, message: '' })}><option value="keyword">키워드 검색</option><option value="semantic">의미 검색 · AI</option><option value="hybrid">통합 검색 · AI</option></select><Button size="icon" variant="ghost" aria-label={compact ? '편안한 목록 밀도' : '촘촘한 목록 밀도'} aria-pressed={compact} disabled={!preferences.data || preferences.locked('ui.density')} onClick={() => preferences.update('ui.density', compact ? 'comfortable' : 'compact')}><ListFilter size={17} /></Button></div>
        {mode === 'keyword' || !query ? <div className="tabs mail-filter-tabs" aria-label="메일 필터">{filters.map(filter => <button type="button" key={filter.id} className={folder === filter.id ? 'active' : ''} aria-pressed={folder === filter.id} onClick={() => change({ folder: filter.id, message: '' })}>{filter.label}</button>)}</div> : <p className="muted">AI 검색은 선택 계정의 메일 전체에서 최대 100건을 찾습니다.</p>}
        {(mode === 'keyword' || !query) && <AdvancedSearch params={params} onChange={values => change({...values, message: ''})}/>}
        {!!messages.length && <label className="row small"><input type="checkbox" aria-label="불러온 메일 전체 선택" checked={messages.every(message => checked.has(message.id))} onChange={event => setChecked(event.target.checked ? new Set(messages.map(message => message.id)) : new Set())}/>불러온 메일 전체 선택 ({messages.length}개)</label>}
        <BulkActions ids={[...checked]} onSuccess={ids => setChecked(previous => new Set([...previous].filter(id => !ids.includes(id))))}/>
      </header>
      {mail.isPending ? <Loading /> : mail.error ? <ErrorState error={mail.error} retry={() => mail.refetch()} /> : !messages.length ? <EmptyState title={query ? '검색 결과가 없습니다' : '아직 받은 메일이 없습니다'} description={query ? '검색어나 검색 방식을 바꿔 다시 찾아보세요.' : '계정에서 동기화 상태를 확인해 주세요. 수집된 메일이 이곳에 표시됩니다.'} /> : <div className="mail-virtual-list">
        <Virtuoso ref={list} data={messages} computeItemKey={(_, message) => message.id} endReached={() => { if (mail.hasNextPage && !mail.isFetchingNextPage) mail.fetchNextPage() }} itemContent={(_, message) => <div className="mail-select-row"><input type="checkbox" aria-label={`${message.subject || '제목 없는 메일'} 선택`} checked={checked.has(message.id)} onChange={event => setChecked(previous => {const next = new Set(previous); if (event.target.checked) next.add(message.id); else next.delete(message.id); return next})}/><button className={`mail-list-item ${message.is_read===false?'is-unread':''} ${selected === message.id ? 'selected' : ''}`} onClick={() => select(message)} aria-current={selected === message.id ? 'true' : undefined}>
          <div className="mail-item-top"><span className="mail-sender">{message.from.name || message.from.email}</span><time>{mailDate(message.date)}</time></div>
          <strong className="mail-subject">{message.subject || '(제목 없음)'}</strong>
          <p className="mail-snippet" style={{WebkitLineClamp:previewLines,display:previewLines===0?'none':undefined}}>{snippets[message.id] || views[message.id]?.body?.text_body?.slice(0, 160) || message.from.email}</p>
          <div className="mail-item-meta">{message.is_important && <Badge variant="outline"><Star size={12} /> 중요</Badge>}{message.has_attachments && <span aria-label="첨부파일 있음"><Paperclip size={13} /> 첨부</span>}{message.labels?.slice(0, 2).map(label => <Badge key={label} variant="secondary">{label}</Badge>)}{views[message.id]?.reason && <span>{views[message.id].reason}</span>}</div>
          {message.is_read===false&&<span className="small muted">안읽음</span>}
        </button></div>} components={{ Footer: () => mail.hasNextPage ? <div className="mail-list-footer"><Button variant="ghost" size="sm" disabled={mail.isFetchingNextPage} onClick={() => mail.fetchNextPage()}><ArrowDown size={15} />{mail.isFetchingNextPage ? '불러오는 중…' : '더 불러오기'}</Button></div> : <p className="mail-list-footer muted">{messages.length}개 메일 · 목록 끝</p> }} />
      </div>}
    </section>
    {reader !== 'hidden' && (selected ? <MessagePane key={selected} id={selected} onClose={() => change({ message: '' })} /> : <div className="mail-welcome"><div className="mail-welcome-icon"><Inbox size={36} strokeWidth={1.4} /></div><h2>메일에서 다음 할 일까지</h2><p>메일을 선택하면 본문과 AI Insight를<br />한곳에서 확인할 수 있습니다.</p><span className="muted">J / K 메일 이동 · 검색에서 의미까지 찾기</span></div>)}
  </div>
}
