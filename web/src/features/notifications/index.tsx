import {useEffect, useRef, useState} from 'react'
import {useQuery, useQueryClient} from '@tanstack/react-query'
import {Link} from 'react-router-dom'
import {toast} from 'sonner'
import {api} from '@/api/client'
import {useSession} from '@/app/session'
import {Badge, ErrorState, Loading} from '@/components/ui'
import {formatDate} from '@/lib/utils'

type Category = 'sync' | 'send' | 'ai' | 'action' | 'security'
export interface NotificationItem {id: string; category: Category; kind: string; resource_id?: string; status: string; at: number}
export interface NotificationSnapshot {user_id: string; enabled: boolean; poll_seconds: number; preferences_revision?: string; events: NotificationItem[]}
type Connection = 'connecting' | 'connected' | 'polling' | 'interrupted'
const categories: Record<Category, string> = {sync: '메일 동기화', send: '메일 발송', ai: 'AI 작업', action: '업무·액션', security: '보안 활동'}
const statuses: Record<string, string> = {queued: '대기', running: '진행 중', succeeded: '완료', partially_succeeded: '일부 완료', failed: '실패', cancelled: '취소', uncertain: '결과 확인 필요', send_uncertain: '발송 결과 확인 필요', pending: '대기', sending: '발송 중', sent: '발송 완료', retry_pending: '재시도 대기', retry_wait: '재시도 대기', approved: '승인됨', rejected: '반려됨', done: '완료', exported: '내보냄', ok: '처리됨', denied: '거부됨', error: '실패', unknown: '확인 필요'}
const snapshotKey = (userID?: string) => ['notifications', userID] as const
const connectionKey = (userID?: string) => ['notification-connection', userID] as const
const label = (event: NotificationItem) => `${categories[event.category]} · ${statuses[event.status] || '확인 필요'}`

function snapshotOptions(userID?: string) {
  return {queryKey: snapshotKey(userID), queryFn: ({signal}: {signal: AbortSignal}) => api<NotificationSnapshot>('/api/events?once=true', {signal}), enabled: Boolean(userID), retry: false as const}
}

export function notificationChanges(previous: NotificationSnapshot | undefined, next: NotificationSnapshot): NotificationItem[] {
  if (!previous?.enabled || !next.enabled || previous.user_id !== next.user_id) return []
  const old = new Map(previous.events.map(event => [event.id, event]))
  return next.events.filter(event => {
    const prior = old.get(event.id)
    return !prior || prior.status !== event.status
  })
}

function validSnapshot(value: unknown, userID: string): value is NotificationSnapshot {
  if (!value || typeof value !== 'object') return false
  const data = value as Partial<NotificationSnapshot>
  return data.user_id === userID && typeof data.enabled === 'boolean' && typeof data.poll_seconds === 'number' && Array.isArray(data.events) && data.events.length <= 250 && data.events.every(event =>
    event && typeof event.id === 'string' && typeof event.at === 'number' && Object.hasOwn(categories, event.category) && Object.hasOwn(statuses, event.status))
}

// Mount once inside the authenticated, identity-keyed QueryClient boundary.
// The browser carries only its HttpOnly cookie; no credential goes in a URL or
// browser storage. Closing/unmounting discards prior-user event state.
export function NotificationEvents() {
  const session = useSession()
  const userID = session?.user_id
  const cache = useQueryClient()
  const [connected, setConnected] = useState(false)
  const previous = useRef<NotificationSnapshot | undefined>(undefined)
  const shown = useRef(new Set<string>())
  const snapshot = useQuery({...snapshotOptions(userID), refetchInterval: connected ? false : (query) => Math.max(5, Math.min(300, query.state.data?.poll_seconds || 15)) * 1000})

  useEffect(() => {
    const next = snapshot.data
    if (!userID || !validSnapshot(next, userID)) return
    const oldRevision = previous.current?.preferences_revision
    if (oldRevision && next.preferences_revision && oldRevision !== next.preferences_revision) void cache.invalidateQueries({queryKey: ['preferences']})
    const changes = notificationChanges(previous.current, next)
    previous.current = next
    // Reconnect bursts are summarized to prevent flooding the workspace.
    if (changes.length > 3) {
      const id = `notifications:${userID}:summary`
      shown.current.add(id)
      toast.info(`새 작업·활동 상태 ${changes.length}건이 있습니다.`, {id})
    } else for (const change of changes) {
      const id = `notifications:${userID}:${change.id}`
      shown.current.add(id)
      if (['failed', 'error', 'uncertain', 'send_uncertain'].includes(change.status)) toast.error(label(change), {id})
      else toast.info(label(change), {id})
    }
    if (changes.length) {
      const affected = new Set(changes.flatMap(event => event.category === 'sync' ? ['jobs', 'messages', 'accounts'] : event.category === 'send' ? ['outbound', 'drafts'] : event.category === 'action' ? ['work', 'team', 'action-cards'] : event.category === 'ai' ? ['jobs'] : []))
      for (const name of affected) void cache.invalidateQueries({queryKey: [name]})
    }
  }, [cache, snapshot.data, userID])

  useEffect(() => {
    if (!userID) return
    let active = true
    previous.current = undefined
    setConnected(false)
    const connection = (value: Connection) => {if (active) cache.setQueryData(connectionKey(userID), value)}
    const displayed = shown.current
    if (typeof EventSource === 'undefined') {
      connection('polling')
      return () => {active = false; previous.current = undefined; for (const id of displayed) toast.dismiss(id); displayed.clear()}
    }
    connection('connecting')
    const source = new EventSource('/api/events')
    source.onopen = () => {if (active) {setConnected(true); connection('connected')}}
    source.onerror = () => {if (active) {setConnected(false); connection('polling')}}
    source.addEventListener('snapshot', event => {
      if (!active) return
      try {
        const value: unknown = JSON.parse((event as MessageEvent<string>).data)
        if (!validSnapshot(value, userID)) return
        cache.setQueryData(snapshotKey(userID), value)
      } catch {connection('interrupted')}
    })
    source.addEventListener('session_expired', () => {
      if (!active) return
      source.close()
      previous.current = undefined
      cache.removeQueries({queryKey: snapshotKey(userID)})
      window.dispatchEvent(new Event('postra:unauthorized'))
    })
    source.addEventListener('stream_error', () => {if (active) {source.close(); setConnected(false); connection('interrupted')}})
    return () => {
      active = false
      source.close()
      previous.current = undefined
      for (const id of displayed) toast.dismiss(id)
      displayed.clear()
    }
  }, [cache, userID])
  return null
}

export function useNotificationSummary() {
	const userID = useSession()?.user_id
	const snapshot = useQuery(snapshotOptions(userID))
	const data = snapshot.data
	return {snapshot, running: data && data.user_id === userID && data.enabled ? data.events.filter(event => event.kind === 'job' && ['queued', 'running'].includes(event.status)).length : 0}
}

export function NotificationsPanel() {
  const userID = useSession()?.user_id
  const {snapshot} = useNotificationSummary()
  const connection = useQuery({queryKey: connectionKey(userID), queryFn: async (): Promise<Connection> => 'connecting', enabled: false, initialData: 'connecting' as Connection})
  if (snapshot.isPending) return <Loading label="내 알림 불러오는 중…"/>
  if (snapshot.error) return <ErrorState error={snapshot.error} retry={() => snapshot.refetch()}/>
  if (!snapshot.data || snapshot.data.user_id !== userID) return null
  if (!snapshot.data.enabled) return <p className="muted">조직 정책에서 알림이 꺼져 있습니다. 작업 화면에서 상태를 확인할 수 있습니다.</p>
  return <div className="stack"><p className="muted" role="status">{connection.data === 'connected' ? '실시간 연결됨' : connection.data === 'interrupted' ? '실시간 연결이 중단되어 주기적으로 다시 조회합니다.' : '주기적으로 상태를 확인하고 있습니다.'} · {snapshot.data.poll_seconds}초 갱신</p>
    {snapshot.data.events.length ? <ul className="job-list">{snapshot.data.events.slice(0, 50).map(event => <li key={event.id}><div className="row between"><strong>{categories[event.category]}</strong><Badge>{statuses[event.status] || '확인 필요'}</Badge></div><small className="muted">{formatDate(event.at)}</small>{event.kind === 'job' && event.resource_id && <Link to={`/jobs/${encodeURIComponent(event.resource_id)}`}>작업 확인</Link>}{event.kind === 'outbound' && <Link to="/sent">발송 상태 확인</Link>}</li>)}</ul> : <p className="muted">선택한 알림 범주에 최근 활동이 없습니다.</p>}
    <Link to="/settings">알림 범주 설정</Link>
  </div>
}
