import { useState, type FormEvent } from 'react';
import { toast } from 'sonner';
import { api } from '@/api/client';
import { Badge, Button, EmptyState, ErrorState, Input, Loading, PageHeader, Panel, Textarea } from '@/components/ui';
import { date, Field, useResource, type MCPKey } from './shared';

export function KeysPanel({ admin = false }: { admin?: boolean }) {
  const path = admin ? '/api/admin/mcp-keys' : '/api/mcp-keys';
  const { data, error, setError, reload } = useResource<{ keys: MCPKey[] | null }>(path);
  const [busy, setBusy] = useState(false);
  const [rawKey, setRawKey] = useState('');
  async function create(e: FormEvent<HTMLFormElement>) {
    e.preventDefault();
    const form = e.currentTarget;
    setBusy(true); setError(undefined); setRawKey('');
    try { const result = await api<{ key: MCPKey; raw_key: string }>('/api/mcp-keys', { method: 'POST', body: { name: new FormData(form).get('name') } }); setRawKey(result.raw_key); form.reset(); reload(); toast.success('API 키를 만들었습니다. 지금 안전한 곳에 복사하세요.'); }
    catch (err) { setError(err); } finally { setBusy(false); }
  }
  async function revoke(key: MCPKey) {
    if (!window.confirm(`${key.name} (${key.key_prefix}) 키를 폐기하시겠습니까? 이 키를 사용하는 연결은 즉시 중단됩니다.`)) return;
    setBusy(true); setError(undefined);
    try { await api(`${path}/${encodeURIComponent(key.id)}`, { method: 'DELETE' }); setRawKey(''); reload(); toast.success('키를 폐기했습니다.'); }
    catch (err) { setError(err); } finally { setBusy(false); }
  }
  async function copy() {
    try { await navigator.clipboard.writeText(rawKey); toast.success('키를 복사했습니다.'); }
    catch { toast.error('자동 복사를 사용할 수 없습니다. 키를 선택하여 직접 복사하세요.'); }
  }
  return <Panel><h2>{admin ? '전체 사용자 MCP 키 관리' : '내 MCP API 키'}</h2><p className="muted">{admin ? '소유자와 키 상태만 확인할 수 있습니다. 기존 키 원문은 다시 조회할 수 없습니다.' : '외부 도구에서 내 메일 워크스페이스에 연결할 때 사용합니다. 생성한 키는 이 화면에서 한 번만 표시합니다.'}</p>
    {!!error && <ErrorState error={error} retry={reload} />}
    {!admin && <form className="row" onSubmit={create}><Field label="새 키 이름"><Input name="name" placeholder="업무 도구 연결" required disabled={busy} /></Field><Button type="submit" disabled={busy}>키 만들기</Button></form>}
    {rawKey && <section className="stack" aria-live="polite"><Field label="새로 만든 API 키" hint="화면을 나가거나 닫으면 다시 볼 수 없습니다. 비밀번호처럼 안전하게 보관하세요."><Textarea value={rawKey} readOnly rows={2} onFocus={e => e.currentTarget.select()} spellCheck={false} /></Field><div className="row"><Button variant="outline" onClick={copy}>키 복사</Button><Button variant="ghost" onClick={() => setRawKey('')}>키 표시 닫기</Button></div></section>}
    {!data ? !error && <Loading /> : !data.keys?.length ? <EmptyState title="등록된 키가 없습니다" /> : <div style={{ overflowX: 'auto' }}><table className="table"><thead><tr><th>이름</th>{admin && <th>소유자 ID</th>}<th>키 식별자</th><th>상태</th><th>최근 사용</th><th>관리</th></tr></thead><tbody>{data.keys.map(key => <tr key={key.id}><td>{key.name}</td>{admin && <td>{key.user_id}</td>}<td><code>{key.key_prefix}</code></td><td><Badge variant={key.status === 'active' ? 'secondary' : 'outline'}>{key.status === 'active' ? '사용 중' : '폐기됨'}</Badge></td><td>{date(key.last_used_at)}</td><td><Button variant="destructive" size="sm" disabled={busy || key.status !== 'active'} onClick={() => revoke(key)}>폐기</Button></td></tr>)}</tbody></table></div>}
  </Panel>;
}

export function MCPKeysPage() { return <div className="page stack"><PageHeader title="연결용 API 키" description="내 계정에 접근할 수 있는 도구 연결을 관리합니다." /><KeysPanel /></div>; }

interface Incident { id: string; severity: string; component: string; message: string; detail?: string; count: number; last_seen: number; resolved: boolean }
interface IncidentResult { incidents: Incident[] | null; stats: { open_critical: number; open_error: number; open_warning: number; open_total: number; resolved: number } }
export function IncidentsPanel() {
  const [severity, setSeverity] = useState(''), [resolved, setResolved] = useState(false);
  const { data, error, setError, reload } = useResource<IncidentResult>(`/api/admin/incidents?limit=100&severity=${severity}&resolved=${resolved}`);
  const [busy, setBusy] = useState(false);
  async function resolve(id: string) {
    setBusy(true); setError(undefined);
    try { await api(`/api/admin/incidents/${encodeURIComponent(id)}/resolve`, { method: 'POST' }); reload(); toast.success('장애를 해결됨으로 표시했습니다.'); }
    catch (err) { setError(err); } finally { setBusy(false); }
  }
  return <div className="stack"><div className="row"><h2>시스템 장애</h2><Button variant="outline" onClick={reload}>새로고침</Button></div>
    {data && <div className="grid"><Panel><strong>미해결 {data.stats.open_total}건</strong></Panel><Panel>심각 {data.stats.open_critical} · 오류 {data.stats.open_error} · 경고 {data.stats.open_warning}</Panel></div>}
    <div className="row"><Field label="심각도"><select className="input" value={severity} onChange={e => setSeverity(e.target.value)}><option value="">전체</option><option value="critical">심각</option><option value="error">오류</option><option value="warning">경고</option></select></Field><label className="row"><input type="checkbox" checked={resolved} onChange={e => setResolved(e.target.checked)} />해결된 항목 포함</label></div>
    {!!error && <ErrorState error={error} retry={reload} />}
    {!data ? !error && <Loading /> : !data.incidents?.length ? <EmptyState title="표시할 장애가 없습니다" /> : data.incidents.map(incident => <Panel key={incident.id}><div className="row"><Badge variant={incident.severity === 'warning' ? 'secondary' : 'destructive'}>{({ critical: '심각', error: '오류', warning: '경고' } as Record<string, string>)[incident.severity] || incident.severity}</Badge><span>{incident.component}</span><span className="muted">{incident.count}회 · {date(incident.last_seen)}</span></div><h3>{incident.message}</h3>{incident.detail && <details><summary>오류 상세 보기</summary><pre style={{ whiteSpace: 'pre-wrap', overflowWrap: 'anywhere' }}>{incident.detail}</pre></details>}<div className="row">{incident.resolved ? <Badge variant="outline">해결됨</Badge> : <Button size="sm" variant="outline" disabled={busy} onClick={() => resolve(incident.id)}>해결됨으로 표시</Button>}</div></Panel>)}
  </div>;
}

interface Audit { id: number; at: number; actor: string; user_id: string; action: string; resource: string; result: string; detail?: string }
export function AuditPanel() {
  const { data, error, reload } = useResource<Audit[] | null>('/api/audit?limit=200');
  return <Panel><div className="row"><h2>감사 기록</h2><Button variant="outline" onClick={reload}>새로고침</Button></div><p className="muted">접근 권한 범위의 최근 200개 작업 기록입니다.</p>{!!error && <ErrorState error={error} retry={reload} />}
    {data === undefined ? !error && <Loading /> : !data?.length ? <EmptyState title="감사 기록이 없습니다" /> : <div style={{ overflowX: 'auto' }}><table className="table"><thead><tr><th>시각</th><th>작업</th><th>대상</th><th>실행 주체</th><th>결과</th></tr></thead><tbody>{data.map(event => <tr key={event.id}><td>{date(event.at)}</td><td>{event.action}{event.detail && <details><summary>상세</summary><p style={{ overflowWrap: 'anywhere' }}>{event.detail}</p></details>}</td><td>{event.resource}</td><td>{event.actor}</td><td><Badge variant={event.result === 'error' || event.result === 'denied' ? 'destructive' : 'secondary'}>{({ ok: '정상', denied: '거부', error: '오류' } as Record<string, string>)[event.result] || event.result}</Badge></td></tr>)}</tbody></table></div>}
  </Panel>;
}
