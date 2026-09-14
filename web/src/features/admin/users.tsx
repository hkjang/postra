import { useState, type FormEvent } from 'react';
import { toast } from 'sonner';
import { api } from '@/api/client';
import { useSession } from '@/app/session';
import { Badge, Button, EmptyState, ErrorState, Input, Loading, Panel } from '@/components/ui';
import { date, Field, useResource, type User } from './shared';

export function UsersPanel() {
  const { data: users, error, setError, reload } = useResource<User[] | null>('/api/admin/users');
  const principal = useSession();
  const [editing, setEditing] = useState<User>();
  const [create, setCreate] = useState(false);
  const [busy, setBusy] = useState(false);
  async function save(e: FormEvent<HTMLFormElement>) {
    e.preventDefault();
    const form = e.currentTarget;
    const data = new FormData(form);
    setBusy(true); setError(undefined);
    try {
      if (editing) {
        await api(`/api/admin/users/${encodeURIComponent(editing.id)}`, { method: 'PATCH', body: { display_name: data.get('display_name'), email: data.get('email'), role: data.get('role'), status: data.get('status') } });
      } else {
        // The existing create endpoint decodes a Go input with no JSON tags.
        await api('/api/admin/users', { method: 'POST', body: { LoginID: data.get('login_id'), DisplayName: data.get('display_name'), Email: data.get('email'), Role: data.get('role'), Password: data.get('password') } });
      }
      toast.success(editing ? '사용자 정보를 저장했습니다.' : '사용자를 만들었습니다.');
      setEditing(undefined); setCreate(false); reload();
    } catch (err) { setError(err); }
    finally { const input = form.elements.namedItem('password'); if (input instanceof HTMLInputElement) input.value = ''; setBusy(false); }
  }
  async function remove(user: User) {
    if (!window.confirm(`${user.email || user.login_id} 사용자를 삭제하시겠습니까? 로그인 연결이 해제되며, 이전 메일은 삭제된 사용자에게 격리되어 보관됩니다.`)) return;
    setBusy(true); setError(undefined);
    try { await api(`/api/admin/users/${encodeURIComponent(user.id)}`, { method: 'DELETE' }); toast.success('사용자를 삭제했습니다.'); if (editing?.id === user.id) setEditing(undefined); reload(); }
    catch (err) { setError(err); } finally { setBusy(false); }
  }
  async function resetPassword(e: FormEvent<HTMLFormElement>) {
    e.preventDefault();
    if (!editing || !window.confirm(`${editing.login_id} 사용자의 비밀번호를 변경하고 기존 세션을 종료하시겠습니까?`)) return;
    const form = e.currentTarget;
    setBusy(true); setError(undefined);
    try { await api(`/api/admin/users/${encodeURIComponent(editing.id)}/password`, { method: 'POST', body: { password: new FormData(form).get('password') } }); toast.success('비밀번호를 변경하고 기존 세션을 종료했습니다.'); }
    catch (err) { setError(err); } finally { form.reset(); setBusy(false); }
  }
  return <div className="stack">
    <div className="row"><h2>사용자 관리</h2><Button onClick={() => { setCreate(true); setEditing(undefined); }}>사용자 추가</Button></div>
    <p className="muted">관리자 권한은 사용자와 정책을 관리하는 권한입니다. 다른 사용자의 메일 계정이나 본문을 조회할 수 없습니다.</p>
    {!!error && <ErrorState error={error} retry={reload} />}
    {(create || editing) && <Panel><h3>{editing ? `${editing.display_name} 정보 수정` : '로컬 사용자 추가'}</h3><form className="stack" onSubmit={save} key={editing?.id || 'create'}>
      <fieldset disabled={busy} className="grid" style={{ border: 0, padding: 0 }}>
        {!editing && <Field label="로그인 아이디"><Input name="login_id" required autoComplete="off" /></Field>}
        <Field label="이름"><Input name="display_name" defaultValue={editing?.display_name || ''} required /></Field>
        <Field label="이메일"><Input type="email" name="email" defaultValue={editing?.email || ''} /></Field>
        <Field label="역할"><select className="input" name="role" defaultValue={editing?.role || 'user'}><option value="user">사용자</option><option value="admin">관리자</option></select></Field>
        {editing ? <Field label="상태"><select className="input" name="status" defaultValue={editing.status}><option value="active">사용</option><option value="disabled">로그인 중지</option></select></Field> : <Field label="초기 비밀번호" hint="12자 이상. 저장 후 다시 조회할 수 없습니다."><Input name="password" type="password" minLength={12} required autoComplete="new-password" /></Field>}
      </fieldset>
      <div className="row"><Button type="submit" disabled={busy}>저장</Button><Button type="button" variant="ghost" onClick={() => { setCreate(false); setEditing(undefined); }}>취소</Button></div>
    </form>
      {editing?.auth_provider === 'local' && <form className="stack" onSubmit={resetPassword}><h3>비밀번호 재설정</h3><Field label="새 비밀번호"><Input name="password" type="password" minLength={12} required autoComplete="new-password" disabled={busy} /></Field><div><Button type="submit" variant="outline" disabled={busy}>비밀번호 재설정</Button></div></form>}
      {editing?.auth_provider === 'oidc' && <p className="muted">SSO 로그인 비밀번호는 Keycloak에서 관리합니다. 메일 비밀번호는 별도의 프로비저닝 설정을 사용합니다.</p>}
    </Panel>}
    {users === undefined ? !error && <Loading /> : !users?.length ? <EmptyState title="등록된 사용자가 없습니다" /> : <Panel><div style={{ overflowX: 'auto' }}><table className="table"><thead><tr><th>사용자</th><th>인증</th><th>역할·상태</th><th>최근 로그인</th><th>관리</th></tr></thead><tbody>{users.map(user => <tr key={user.id}>
      <td><strong>{user.display_name}</strong><div className="muted">{user.email || user.login_id}</div></td><td>{user.auth_provider === 'oidc' ? 'Keycloak SSO' : '로컬'}</td>
      <td><Badge variant="secondary">{user.role === 'admin' ? '관리자' : '사용자'}</Badge> {user.status === 'active' ? '사용 중' : '중지됨'}</td><td>{date(user.last_login_at)}</td>
      <td><div className="row"><Button variant="outline" size="sm" disabled={busy} onClick={() => { setEditing(user); setCreate(false); }}>수정</Button><Button variant="destructive" size="sm" disabled={busy || user.id === principal?.user_id} onClick={() => remove(user)}>삭제</Button></div></td>
    </tr>)}</tbody></table></div></Panel>}
  </div>;
}

export function ProvisioningPanel() {
  const [specific, setSpecific] = useState(false);
  const [same, setSame] = useState(true);
  const [auth, setAuth] = useState('none');
  const [imapSecurity, setIMAPSecurity] = useState('tls');
  const [smtpSecurity, setSMTPSecurity] = useState('starttls');
  const [imapPort, setIMAPPort] = useState('993');
  const [smtpPort, setSMTPPort] = useState('587');
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<unknown>();
  const [success, setSuccess] = useState('');
  async function provision(e: FormEvent<HTMLFormElement>) {
    e.preventDefault();
    const form = e.currentTarget, data = new FormData(form);
    const shared = !specific || data.get('apply_to_all_users') === 'on';
    if (shared && !window.confirm('조직 공통 SSO 메일 정책을 저장하시겠습니까? 이 정책은 SSO 사용자가 로그인할 때 자신의 이메일로 메일 계정을 자동 연결하는 데 사용됩니다.')) return;
    setBusy(true); setError(undefined); setSuccess('');
    try {
      await api('/api/admin/mail-accounts', { method: 'POST', body: {
        email: specific ? data.get('email') : '', imap_host: data.get('imap_host'), imap_port: Number(imapPort), imap_security: imapSecurity,
        smtp_host: data.get('smtp_host'), smtp_port: Number(smtpPort), smtp_security: smtpSecurity, smtp_auth: auth,
        auth_username: specific ? data.get('auth_username') : '', mail_password: data.get('mail_password'), same_password: same,
        smtp_password: auth === 'none' || same ? '' : data.get('smtp_password'), automatic_sync: data.get('automatic_sync') === 'on',
        apply_to_all_users: shared, insecure_skip_verify: data.get('insecure_skip_verify') === 'on',
      } });
      setSuccess(specific ? '이메일에 연결된 사용자 메일 계정을 프로비저닝했습니다.' : '조직 공통 정책을 저장했습니다. SSO 로그인 시 자신의 이메일로 메일 계정이 자동 연결됩니다.');
      toast.success('메일 프로비저닝 설정을 저장했습니다.');
    } catch (err) { setError(err); }
    finally { for (const name of ['mail_password', 'smtp_password']) { const input = form.elements.namedItem(name); if (input instanceof HTMLInputElement) input.value = ''; } setBusy(false); }
  }
  return <Panel><h2>SSO 사용자 메일 프로비저닝</h2><p className="muted">기본값은 대상 사용자를 지정하지 않는 조직 공통 정책입니다. 각 사용자의 SSO 이메일과 관리자가 입력한 공통 메일 비밀번호로 IMAP을 연결합니다.</p>
    {!!error && <ErrorState error={error} />}{success && <p role="status">{success}</p>}
    <form className="stack" onSubmit={provision}><fieldset disabled={busy} className="stack" style={{ border: 0, padding: 0 }}>
      <label className="row"><input type="checkbox" checked={specific} onChange={e => setSpecific(e.target.checked)} />특정 이메일 사용자에게 먼저 적용</label>
      {specific && <><Field label="대상 사용자 이메일" hint="이메일 주소만 입력하세요. Keycloak 이메일과 동일해야 합니다."><Input name="email" type="email" placeholder="hong@corp.local" required /></Field><label className="row"><input name="apply_to_all_users" type="checkbox" />이 설정을 모든 SSO 사용자에게 적용할 공통 정책으로도 저장</label></>}
      <div className="grid">
        <Field label="IMAP 서버"><Input name="imap_host" placeholder="imap.corp.local" required /></Field>
        <Field label="IMAP 보안"><select className="input" value={imapSecurity} onChange={e => { setIMAPSecurity(e.target.value); setIMAPPort(e.target.value === 'tls' ? '993' : '143'); }}><option value="tls">TLS</option><option value="starttls">STARTTLS</option><option value="none">없음 · 격리망</option></select></Field>
        <Field label="IMAP 포트"><Input type="number" min={1} max={65535} value={imapPort} onChange={e => setIMAPPort(e.target.value)} required /></Field>
        <Field label="SMTP 서버"><Input name="smtp_host" placeholder="smtp.corp.local" required /></Field>
        <Field label="SMTP 보안"><select className="input" value={smtpSecurity} onChange={e => { setSMTPSecurity(e.target.value); setSMTPPort(e.target.value === 'tls' ? '465' : '587'); }}><option value="tls">TLS</option><option value="starttls">STARTTLS</option><option value="none">없음 · 격리망</option></select></Field>
        <Field label="SMTP 포트"><Input type="number" min={1} max={65535} value={smtpPort} onChange={e => setSMTPPort(e.target.value)} required /></Field>
        <Field label="SMTP 인증"><select className="input" value={auth} onChange={e => setAuth(e.target.value)}><option value="none">인증 없음 · 내부 릴레이</option><option value="auto">아이디·비밀번호 인증</option></select></Field>
        {specific && <Field label="대상 사용자의 메일 인증 아이디" hint="비워 두면 대상 이메일을 사용합니다. 조직 공통 정책은 각 사용자의 SSO 이메일로 인증합니다."><Input name="auth_username" placeholder="hong@corp.local" autoComplete="off" /></Field>}
        <Field label="메일 비밀번호" hint="Keycloak 로그인 비밀번호와는 별개입니다. 저장 후 재조회할 수 없습니다."><Input name="mail_password" type="password" required autoComplete="new-password" /></Field>
      </div>
      <label className="row"><input type="checkbox" checked={same} onChange={e => setSame(e.target.checked)} />IMAP·SMTP에 동일한 비밀번호 적용</label>
      {auth !== 'none' && !same && <Field label="SMTP 비밀번호"><Input name="smtp_password" type="password" required autoComplete="new-password" /></Field>}
      {auth === 'none' && <p className="muted">SMTP 비밀번호 없이 내부 릴레이를 사용합니다. SMTP 인증 명령을 보내지 않습니다.</p>}
      <label className="row"><input type="checkbox" name="automatic_sync" defaultChecked />자동 메일 수집 활성화</label>
      <p className="muted">해제하면 새로 생성되는 메일 계정은 중지 상태가 됩니다. 수집과 발송을 사용하려면 계정을 다시 활성화해야 합니다.</p>
      <label className="row"><input type="checkbox" name="insecure_skip_verify" />TLS 인증서 검증 생략</label>
      <p className="muted">사설 서버·평문·무인증 연결은 보안 정책에서 허용되어야 합니다. 저장된 정책이나 비밀번호를 이 화면에서 다시 읽어 오지 않습니다.</p>
      <div><Button type="submit" disabled={busy}>{busy ? '저장 중…' : '프로비저닝 설정 저장'}</Button></div>
    </fieldset></form>
  </Panel>;
}

export function PurgePanel() {
  const [busy, setBusy] = useState(false), [error, setError] = useState<unknown>();
  async function purge(e: FormEvent<HTMLFormElement>) {
    e.preventDefault();
    const form = e.currentTarget, data = new FormData(form);
    const email = String(data.get('email') || '').trim();
    if (email.toLowerCase() !== String(data.get('confirmation') || '').trim().toLowerCase()) { setError(new Error('두 이메일 주소가 일치하지 않습니다.')); return; }
    if (!window.confirm(`${email}에 해당하는 삭제된 사용자의 이전 메일 데이터를 영구 삭제합니다. 이 작업은 복구할 수 없습니다. 계속하시겠습니까?`)) return;
    setBusy(true); setError(undefined);
    try { await api('/api/admin/deleted-mail/purge', { method: 'POST', body: { email, confirmation: data.get('confirmation') } }); toast.success('삭제된 사용자의 이전 메일 데이터를 영구 삭제했습니다.'); form.reset(); }
    catch (err) { setError(err); } finally { setBusy(false); }
  }
  return <Panel><h2>삭제된 사용자의 이전 메일 정리</h2><p className="muted">이미 삭제된 사용자에게 남아 있는 메일 데이터만 이메일 기준으로 영구 삭제합니다. 현재 활성 사용자의 메일은 대상에 포함되지 않습니다.</p>{!!error && <ErrorState error={error} />}<form className="stack" onSubmit={purge}><div className="grid"><Field label="이전 사용자 이메일"><Input name="email" type="email" required disabled={busy} /></Field><Field label="이메일 다시 입력"><Input name="confirmation" type="email" required disabled={busy} /></Field></div><div><Button type="submit" variant="destructive" disabled={busy}>이전 메일 영구 삭제</Button></div></form></Panel>;
}
