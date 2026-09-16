import {useState, type FormEvent} from 'react'
import {useQuery} from '@tanstack/react-query'
import {toast} from 'sonner'
import {z} from 'zod'
import {api} from '@/api/client'
import {nullableList, parseResponse} from '@/api/response'
import {useSession} from '@/app/session'
import {Badge, Button, EmptyState, ErrorState, Input, Loading, PageHeader, Panel, Textarea} from '@/components/ui'
import {MCPConnectionPanel} from './ConnectionPanel'

const keysSchema = z.object({keys: nullableList(z.object({id: z.string(), user_id: z.string(), name: z.string(), key_prefix: z.string(), status: z.string(),
  scopes: nullableList(z.string()), legacy_scopes: z.boolean().optional(), last_used_at: z.number().optional(),
}))})
type MCPKey = z.output<typeof keysSchema>['keys'][number]
function keyList(value: unknown) {
  return parseResponse(keysSchema, value)
}
const capabilitiesSchema = z.object({enabled: z.boolean(), http_enabled: z.boolean(), endpoint: z.string(),
  permissions: z.record(z.string(), z.boolean()), groups: z.record(z.string(), nullableList(z.string())), aliases: z.record(z.string(), z.string()), request_timeout_sec: z.number().int().nonnegative(),
})
const scopes = [
  ['mail.read', '메일 읽기'], ['mail.search', '검색'], ['mail.ai', 'AI 분석'], ['mail.draft', '초안·서식·서명'],
  ['mail.send', '승인된 메일 발송'], ['mail.delete', '메일 삭제'], ['mail.work', '분류·규칙·업무'], ['admin.read', '관리 정보 읽기'], ['admin.write', '관리 설정 변경'],
] as const

function ScopeSelector({value, onChange, disabled, admin, label}: {value: string[]; onChange: (value: string[]) => void; disabled?: boolean; admin: boolean; label: string}) {
  return <fieldset className="stack" disabled={disabled}><legend>{label}</legend><div className="grid">{scopes.filter(([scope]) => admin || !scope.startsWith('admin.')).map(([scope, title]) => <label key={scope} className="row"><input type="checkbox" checked={value.includes(scope)} onChange={event => onChange(event.target.checked ? [...value, scope] : value.filter(item => item !== scope))}/><span>{title} <small className="muted">({scope})</small></span></label>)}</div><p className="small muted">권한을 선택해도 사용자 역할과 관리자의 MCP 허용 정책을 넘을 수 없습니다. 발송은 별도 미리보기·승인이 필요합니다.</p></fieldset>
}

export function MCPKeysPanel({admin = false}: {admin?: boolean}) {
  const principal = useSession()
  const path = admin ? '/api/admin/mcp-keys' : '/api/mcp-keys'
  const query = useQuery({queryKey: ['mcp-keys', admin], queryFn: async ({signal}) => keyList(await api<unknown>(path, {signal}))})
  const [name, setName] = useState('')
  const [selected, setSelected] = useState<string[]>(['mail.read', 'mail.search'])
  const [editing, setEditing] = useState<MCPKey>()
  const [editScopes, setEditScopes] = useState<string[]>([])
  const [rawKey, setRawKey] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<unknown>()
  async function create(event: FormEvent) {
    event.preventDefault(); setBusy(true); setError(undefined); setRawKey('')
    try {
      const result = await api<{key: MCPKey; raw_key: string}>('/api/mcp-keys', {method: 'POST', body: {name: name.trim(), scopes: selected}})
      setRawKey(result.raw_key); setName(''); await query.refetch(); toast.success('키를 발급했습니다. 원문은 지금 한 번만 확인할 수 있습니다.')
    } catch (failure) { setError(failure) } finally { setBusy(false) }
  }
  async function save() {
    if (!editing || !window.confirm(`${editing.name} 키의 권한을 변경하시겠습니까? 제거한 권한은 열린 MCP 연결에도 즉시 적용됩니다.${editScopes.length ? '' : ' 모든 도구 접근 권한이 제거됩니다.'}`)) return
    setBusy(true); setError(undefined)
    try { await api(`${path}/${encodeURIComponent(editing.id)}`, {method: 'PATCH', body: {scopes: editScopes}}); setEditing(undefined); await query.refetch(); toast.success('MCP 키 권한을 변경했습니다.') }
    catch (failure) { setError(failure) } finally { setBusy(false) }
  }
  async function revoke(key: MCPKey) {
    if (!window.confirm(`${key.name} (${key.key_prefix}) 키를 폐기하시겠습니까? 이 키의 연결은 즉시 중단됩니다.`)) return
    setBusy(true); setError(undefined)
    try { await api(`${path}/${encodeURIComponent(key.id)}`, {method: 'DELETE'}); setRawKey(''); setEditing(undefined); await query.refetch(); toast.success('MCP 키를 폐기했습니다.') }
    catch (failure) { setError(failure) } finally { setBusy(false) }
  }
  async function copy() {
    try { await navigator.clipboard.writeText(rawKey); toast.success('키를 복사했습니다.') }
    catch { toast.error('키를 선택하여 직접 복사하세요.') }
  }
  if (query.error) return <Panel><h2>{admin ? '전체 사용자 MCP 키' : '내 MCP 키'}</h2><ErrorState error={query.error} retry={() => query.refetch()}/></Panel>
  return <Panel className="stack"><h2>{admin ? '전체 사용자 MCP 키' : '내 MCP 키'}</h2><p className="muted">키별로 필요한 최소 권한을 부여하세요. 기존 키 원문은 다시 조회할 수 없으며, 생성된 원문을 브라우저 저장소에 보관하지 않습니다.</p>
    {(Boolean(error) || query.error) && <ErrorState error={error || query.error} retry={() => {setError(undefined); void query.refetch()}}/>}
    {!admin && <form className="stack" onSubmit={create}><label className="field">클라이언트·키 이름<Input value={name} onChange={event => setName(event.target.value)} placeholder="회사 AI 도구" maxLength={120} required disabled={busy}/></label><ScopeSelector label="새 키 권한" value={selected} onChange={setSelected} disabled={busy} admin={principal?.role === 'admin'}/><div><Button disabled={busy || !name.trim()} type="submit">키 발급</Button></div></form>}
    {rawKey && <section className="stack" aria-live="polite"><label className="field">새로 발급한 키 — 한 번만 표시<Textarea readOnly value={rawKey} spellCheck={false} rows={2} onFocus={event => event.currentTarget.select()}/></label><p className="small muted">화면을 닫으면 원문은 다시 확인할 수 없습니다. 안전한 비밀 저장소에 보관하세요.</p><div className="row"><Button variant="outline" onClick={copy}>키 복사</Button><Button variant="ghost" onClick={() => setRawKey('')}>키 표시 닫기</Button></div></section>}
    {query.isPending ? <Loading/> : !query.data?.keys?.length ? <EmptyState title="등록된 MCP 키가 없습니다"/> : <div style={{overflowX: 'auto'}}><table className="table"><thead><tr><th>이름·식별자</th>{admin && <th>소유자 ID</th>}<th>허용 범위</th><th>상태</th><th>관리</th></tr></thead><tbody>{query.data.keys.map(key => <tr key={key.id}><td>{key.name}<br/><code>{key.key_prefix}</code>{key.legacy_scopes && <p><Badge variant="outline">기존 키 호환 권한</Badge></p>}</td>{admin && <td>{key.user_id}</td>}<td>{key.scopes?.length ? key.scopes.join(', ') : '권한 없음'}{key.legacy_scopes && <p className="small muted">기존 업무 권한을 보존했습니다. 사용 범위를 검토하고 줄일 수 있습니다.</p>}</td><td><Badge variant={key.status === 'active' ? 'secondary' : 'outline'}>{key.status === 'active' ? '사용 중' : '폐기됨'}</Badge>{!!key.last_used_at && <p className="small muted">{new Date(key.last_used_at * 1000).toLocaleString('ko-KR')}</p>}</td><td><div className="row"><Button size="sm" variant="outline" disabled={busy || key.status !== 'active'} onClick={() => {setEditing(key); setEditScopes([...key.scopes])}}>권한 편집</Button><Button size="sm" variant="destructive" disabled={busy || key.status !== 'active'} onClick={() => revoke(key)}>폐기</Button></div></td></tr>)}</tbody></table></div>}
    {editing && <section className="stack" aria-label="MCP 키 권한 편집"><h3>{editing.name} 권한 편집</h3><ScopeSelector label="변경할 권한" value={editScopes} onChange={setEditScopes} disabled={busy} admin={principal?.role === 'admin'}/><div className="row"><Button onClick={save} disabled={busy}>권한 저장</Button><Button variant="ghost" disabled={busy} onClick={() => setEditing(undefined)}>취소</Button></div></section>}
  </Panel>
}

export function MCPKeysPage() { return <div className="page stack"><PageHeader title="도구 연결·MCP 키" description="외부 AI 도구가 내 메일에 접근할 수 있는 범위를 관리합니다."/><MCPConnectionPanel/><MCPKeysPanel/></div> }

function MCPAdminContents() {
  const query = useQuery({queryKey: ['mcp-capabilities'], queryFn: async ({signal}) => parseResponse(capabilitiesSchema, await api<unknown>('/api/admin/mcp', {signal}))})
  const value = query.data
  return <div className="stack"><PageHeader title="MCP 운영·권한" description="활성화 상태는 설정값입니다. 실제 연결과 발송 성공을 의미하지 않습니다." actions={<Button variant="outline" onClick={() => query.refetch()}>새로고침</Button>}/>
    <MCPConnectionPanel/>
    {query.error ? <ErrorState error={query.error} retry={() => query.refetch()}/> : !value ? <Loading/> : <>
      <Panel><div className="row"><Badge variant={value.enabled ? 'secondary' : 'outline'}>MCP {value.enabled ? '활성' : '비활성'}</Badge><Badge variant={value.http_enabled ? 'secondary' : 'outline'}>HTTP {value.http_enabled ? '허용' : '차단'}</Badge></div><p>요청 제한: {value.request_timeout_sec}초</p></Panel>
      <Panel><h2>조직 공통 허용 범위</h2><div className="grid">{scopes.map(([scope, label]) => <div className="row" key={scope}><span>{label}</span><Badge variant={value.permissions[scope] ? 'secondary' : 'outline'}>{value.permissions[scope] ? '허용' : '차단'}</Badge></div>)}</div><p className="small muted">조직 공통 설정, 클라이언트 인증 권한, 사용자 역할 정책을 모두 통과해야 실행됩니다. 설정 변경은 관리자 MCP 설정에서 수행합니다.</p></Panel>
      <Panel><h2>기능·도구 목록</h2>{Object.entries(value.groups).map(([group, tools]) => <details key={group}><summary>{group} ({tools.length})</summary><ul>{tools.map(tool => <li key={tool}><code>{tool}</code></li>)}</ul></details>)}<details><summary>호환 별칭</summary>{Object.entries(value.aliases).map(([alias, name]) => <p key={alias}><code>{alias}</code> → <code>{name}</code></p>)}</details><p className="small muted">클라이언트의 tools/list에서 JSON Schema, 부작용, 승인 필요 여부, 필수 권한과 예제를 조회할 수 있습니다. 긴 작업은 반환된 작업 ID로 상태를 확인하세요.</p></Panel>
    </>}<MCPKeysPanel admin/>
  </div>
}

export function MCPAdminPage() {
  const principal = useSession()
  return principal?.role === 'admin' ? <MCPAdminContents/> : <EmptyState title="관리자 권한이 필요합니다"/>
}
