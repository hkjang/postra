import { useCallback, useEffect, useState, type FormEvent, type ReactNode } from 'react';
import { Link, useNavigate, useParams } from 'react-router-dom';
import { toast } from 'sonner';
import { api } from '@/api/client';
import { Badge, Button, EmptyState, ErrorState, Input, Loading, PageHeader, Panel } from '@/components/ui';

interface Account {
  id: string; name: string; email: string; status: string; inbound_protocol?: string;
  pop3_host: string; pop3_port: number; pop3_security: string; pop3_username: string; pop3_secret_ref?: string;
  smtp_host: string; smtp_port: number; smtp_security: string; smtp_username: string; smtp_auth: string; smtp_secret_ref?: string;
  insecure_skip_verify?: boolean; updated_at: number;
}
interface Diagnostic { target: string; ok: boolean; steps: { step: string; ok: boolean; detail?: string }[] }
interface Job { id: string; status: string; progress?: string; error?: string; stats?: Record<string, number> }
const labels: Record<string, string> = { active: '사용 중', disabled: '중지됨', credential_error: '인증 확인 필요', queued: '대기', running: '수집 중', succeeded: '완료', partially_succeeded: '일부 완료', failed: '실패', cancelled: '취소됨' };

function Field({ label, children, hint }: { label: string; children: ReactNode; hint?: string }) {
  return <label className="field"><span>{label}</span>{children}{hint && <small className="muted">{hint}</small>}</label>;
}
function SecuritySelect({ value, onChange, name }: { value: string; onChange: (value: string) => void; name: string }) {
  return <select className="input" name={name} value={value} onChange={e => onChange(e.target.value)}><option value="tls">TLS</option><option value="starttls">STARTTLS</option><option value="none">보안 연결 없음 · 격리망</option></select>;
}
function StateBadge({ status }: { status: string }) {
  return <Badge variant={status === 'credential_error' || status === 'failed' ? 'destructive' : 'secondary'}>{labels[status] || status}</Badge>;
}

function AccountForm({ account, onSaved }: { account?: Account; onSaved: (account: Account) => void }) {
  const [protocol, setProtocol] = useState(account?.inbound_protocol || 'imap');
  const [inSecurity, setInSecurity] = useState(account?.pop3_security || 'tls');
  const [outSecurity, setOutSecurity] = useState(account?.smtp_security || 'starttls');
  const [inPort, setInPort] = useState(String(account?.pop3_port || 993));
  const [outPort, setOutPort] = useState(String(account?.smtp_port || 587));
  const [smtpAuth, setSMTPAuth] = useState(account?.smtp_auth || 'auto');
  const [same, setSame] = useState(!account || account.pop3_secret_ref === account.smtp_secret_ref);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<unknown>();
  const portFor = (nextProtocol: string, security: string) => security === 'tls' ? (nextProtocol === 'imap' ? 993 : 995) : (nextProtocol === 'imap' ? 143 : 110);
  async function submit(e: FormEvent<HTMLFormElement>) {
    e.preventDefault();
    const form = e.currentTarget;
    const values = new FormData(form);
    const password = String(values.get('password') || '');
    const smtpPassword = String(values.get('smtp_password') || '');
    const payload: Record<string, unknown> = {
      name: values.get('name'), email: values.get('email'), inbound_protocol: protocol,
      pop3_host: values.get('pop3_host'), pop3_port: Number(inPort), pop3_security: inSecurity, pop3_username: values.get('pop3_username'),
      smtp_host: values.get('smtp_host'), smtp_port: Number(outPort), smtp_security: outSecurity,
      smtp_auth: smtpAuth, smtp_username: smtpAuth === 'none' ? '' : values.get('smtp_username'),
      insecure_skip_verify: values.get('insecure_skip_verify') === 'on',
    };
    if (account) payload.status = values.get('status');
    setBusy(true); setError(undefined);
    try {
      let inboundRef = account?.pop3_secret_ref;
      if (password) {
        const result = await api<{ secret_ref: string }>('/api/secrets', { method: 'POST', body: { type: 'mail_password', label: `${values.get('email')} 수신 비밀번호`, value: password } });
        inboundRef = result.secret_ref;
        payload.pop3_secret_ref = inboundRef;
      }
      if (smtpAuth === 'none') payload.smtp_secret_ref = '';
      else if (same && inboundRef) payload.smtp_secret_ref = inboundRef;
      else if (smtpPassword) {
        const result = await api<{ secret_ref: string }>('/api/secrets', { method: 'POST', body: { type: 'mail_password', label: `${values.get('email')} SMTP 비밀번호`, value: smtpPassword } });
        payload.smtp_secret_ref = result.secret_ref;
      }
      const saved = await api<Account>(account ? `/api/accounts/${encodeURIComponent(account.id)}` : '/api/accounts', { method: account ? 'PATCH' : 'POST', body: payload });
      toast.success(account ? '메일 계정을 저장했습니다.' : '메일 계정을 연결했습니다.');
      onSaved(saved);
    } catch (err) { setError(err); }
    finally {
      // Passwords are write-only and never restored after a failed request.
      for (const name of ['password', 'smtp_password']) {
        const input = form.elements.namedItem(name);
        if (input instanceof HTMLInputElement) input.value = '';
      }
      setBusy(false);
    }
  }
  return <form className="stack" onSubmit={submit}>
    {!!error && <ErrorState error={error} />}
    <fieldset disabled={busy} className="stack" style={{ border: 0, padding: 0, margin: 0 }}>
      <div className="grid">
        <Field label="계정 이름"><Input name="name" defaultValue={account?.name || ''} placeholder="업무 메일" required /></Field>
        <Field label="메일 주소"><Input name="email" type="email" defaultValue={account?.email || ''} placeholder="hong@corp.local" required /></Field>
        {account && <Field label="계정 상태"><select className="input" name="status" defaultValue={account.status === 'disabled' ? 'disabled' : 'active'}><option value="active">사용</option><option value="disabled">중지</option></select></Field>}
      </div>
      <h3>수신 서버</h3>
      <div className="grid">
        <Field label="수신 방식"><select className="input" value={protocol} onChange={e => { setProtocol(e.target.value); setInPort(String(portFor(e.target.value, inSecurity))); }}><option value="imap">IMAP</option><option value="pop3">POP3</option></select></Field>
        <Field label="수신 서버 주소"><Input name="pop3_host" defaultValue={account?.pop3_host || ''} placeholder="imap.corp.local" required /></Field>
        <Field label="수신 보안"><SecuritySelect name="pop3_security" value={inSecurity} onChange={value => { setInSecurity(value); setInPort(String(portFor(protocol, value))); }} /></Field>
        <Field label="수신 포트"><Input type="number" min={1} max={65535} value={inPort} onChange={e => setInPort(e.target.value)} required /></Field>
        <Field label="수신 인증 아이디"><Input name="pop3_username" defaultValue={account?.pop3_username || ''} placeholder="hong@corp.local" required autoComplete="off" /></Field>
        <Field label={account ? '새 수신 비밀번호' : '수신 비밀번호'} hint={account ? '비워 두면 기존 비밀번호를 유지합니다. 저장된 비밀번호는 조회할 수 없습니다.' : '비밀번호는 암호화하여 저장합니다.'}><Input name="password" type="password" autoComplete="new-password" required={!account} /></Field>
      </div>
      <h3>발송 서버</h3>
      <div className="grid">
        <Field label="SMTP 서버 주소"><Input name="smtp_host" defaultValue={account?.smtp_host || ''} placeholder="smtp.corp.local" required /></Field>
        <Field label="SMTP 보안"><SecuritySelect name="smtp_security" value={outSecurity} onChange={value => { setOutSecurity(value); setOutPort(value === 'tls' ? '465' : '587'); }} /></Field>
        <Field label="SMTP 포트"><Input type="number" min={1} max={65535} value={outPort} onChange={e => setOutPort(e.target.value)} required /></Field>
        <Field label="SMTP 인증"><select className="input" value={smtpAuth} onChange={e => setSMTPAuth(e.target.value)}><option value="auto">아이디·비밀번호 인증</option><option value="none">인증 없음 · 내부 릴레이</option></select></Field>
      </div>
      {smtpAuth !== 'none' ? <>
        <Field label="SMTP 인증 아이디"><Input name="smtp_username" defaultValue={account?.smtp_username || account?.pop3_username || ''} placeholder="hong@corp.local" required autoComplete="off" /></Field>
        <label className="row"><input type="checkbox" checked={same} onChange={e => setSame(e.target.checked)} />수신·발송에 동일한 비밀번호 사용</label>
        {!same && <Field label={account ? '새 SMTP 비밀번호' : 'SMTP 비밀번호'} hint={account ? '기존에 별도 비밀번호를 사용했다면 비워 두어 유지할 수 있습니다. 공통 비밀번호에서 분리하려면 새 값을 입력하세요.' : undefined}><Input type="password" name="smtp_password" autoComplete="new-password" required={!account || !account.smtp_secret_ref || account.smtp_secret_ref === account.pop3_secret_ref} /></Field>}
      </> : <p className="muted">내부 릴레이로 발송합니다. SMTP 비밀번호를 전달하거나 인증 명령을 보내지 않습니다.</p>}
      <label className="row"><input type="checkbox" name="insecure_skip_verify" defaultChecked={account?.insecure_skip_verify} />TLS 인증서 검증 생략</label>
      <p className="muted">사설 서버, 평문 연결, 인증 없는 SMTP는 관리자의 격리망 정책이 허용한 경우 사용할 수 있습니다.</p>
      <div><Button type="submit" disabled={busy}>{busy ? '저장 중…' : account ? '변경 저장' : '계정 연결'}</Button></div>
    </fieldset>
  </form>;
}

export function AccountsPage() {
  const [accounts, setAccounts] = useState<Account[]>();
  const [error, setError] = useState<unknown>();
  const [create, setCreate] = useState(false);
  const navigate = useNavigate();
  const load = useCallback(() => { setError(undefined); api<Account[] | null>('/api/accounts').then(rows => setAccounts(rows || [])).catch(setError); }, []);
  useEffect(load, [load]);
  return <div className="page stack">
    <PageHeader title="메일 계정" description="내 메일 계정의 연결과 수집 상태를 관리합니다." actions={<Button onClick={() => setCreate(!create)}>{create ? '닫기' : '계정 연결'}</Button>} />
    {!!error && <ErrorState error={error} retry={load} />}
    {create && <Panel><h2>새 메일 계정 연결</h2><AccountForm onSaved={account => navigate(`/accounts/${account.id}`)} /></Panel>}
    {!accounts ? !error && <Loading /> : accounts.length === 0 ? <EmptyState title="연결된 메일 계정이 없습니다" description="계정을 직접 연결하거나 관리자의 SSO 메일 프로비저닝 정책을 확인하세요." action={<Button onClick={() => setCreate(true)}>계정 연결</Button>} /> : <div className="grid">{accounts.map(account => <Panel key={account.id}>
      <div className="row"><h2><Link to={`/accounts/${account.id}`}>{account.name || account.email}</Link></h2><StateBadge status={account.status} /></div>
      <p>{account.email}</p><p className="muted">{(account.inbound_protocol || 'pop3').toUpperCase()} · {account.pop3_host}:{account.pop3_port}</p>
      <Link to={`/accounts/${account.id}`}>연결 설정 및 수집 관리 →</Link>
    </Panel>)}</div>}
  </div>;
}

export function AccountDetailPage() {
  const { id = '' } = useParams();
  const [account, setAccount] = useState<Account>();
  const [error, setError] = useState<unknown>();
  const [busy, setBusy] = useState(false);
  const [diagnostics, setDiagnostics] = useState<Diagnostic[]>();
  const [job, setJob] = useState<Job>();
  const navigate = useNavigate();
  const path = `/api/accounts/${encodeURIComponent(id)}`;
  const load = useCallback(() => { setError(undefined); api<Account>(path).then(setAccount).catch(setError); }, [path]);
  useEffect(() => { setAccount(undefined); setJob(undefined); setDiagnostics(undefined); load(); }, [load]);
  useEffect(() => {
    if (!job || !['queued', 'running'].includes(job.status)) return;
    const timer = window.setTimeout(() => { api<Job>(`/api/jobs/${encodeURIComponent(job.id)}`).then(setJob).catch(setError); }, 2000);
    return () => window.clearTimeout(timer);
  }, [job]);
  async function run(action: 'test' | 'sync' | 'delete') {
    if (action === 'delete' && !window.confirm(`${account?.email} 메일 계정 연결을 삭제하시겠습니까? 기존 메일과 감사 기록은 보존됩니다.`)) return;
    setBusy(true); setError(undefined);
    try {
      if (action === 'test') setDiagnostics(await api<Diagnostic[]>(path + '/test', { method: 'POST' }));
      if (action === 'sync') { setJob(await api<Job>(path + '/sync', { method: 'POST', body: {} })); toast.success('메일 수집을 시작했습니다.'); }
      if (action === 'delete') { await api(path, { method: 'DELETE' }); toast.success('계정 연결을 삭제했습니다.'); navigate('/accounts'); }
    } catch (err) { setError(err); } finally { setBusy(false); }
  }
  return <div className="page stack">
    <PageHeader title={account?.name || '메일 계정 설정'} description={account?.email} actions={<><Link to={`/accounts/${id}/preferences`}>동기화·서명·개인화</Link><Link to="/accounts">계정 목록</Link></>} />
    {!!error && <ErrorState error={error} retry={load} />}
    {!account ? !error && <Loading /> : <>
      <Panel><div className="row"><StateBadge status={account.status} /><Button variant="outline" disabled={busy} onClick={() => run('test')}>연결 테스트</Button><Button disabled={busy || account.status !== 'active' || !!job && ['queued', 'running'].includes(job.status)} onClick={() => run('sync')}>지금 메일 수집</Button></div></Panel>
      {job && <Panel><h2>최근 수집 작업</h2><div className="row"><StateBadge status={job.status} /><span>{job.progress}</span></div>{job.error && <ErrorState error={new Error(job.error)} />}{job.stats && <dl className="grid">{Object.entries(job.stats).map(([key, value]) => <div key={key}><dt>{({ new: '새 메일', seen: '확인한 메일', duplicate: '중복', failed: '실패', oversize: '크기 초과', parse_error: '본문 해석 오류' } as Record<string, string>)[key] || key}</dt><dd>{value}</dd></div>)}</dl>}</Panel>}
      {diagnostics && <Panel><h2>연결 진단</h2>{diagnostics.map(diag => <section key={diag.target}><h3>{diag.target.toUpperCase()} · {diag.ok ? '정상' : '확인 필요'}</h3><ul>{(diag.steps || []).map((step, index) => <li key={index}>{step.ok ? '정상' : '실패'} · {step.step}{step.detail && <span> — {step.detail}</span>}</li>)}</ul></section>)}</Panel>}
      <Panel><AccountForm key={`${account.id}:${account.updated_at}`} account={account} onSaved={setAccount} /></Panel>
      <Panel><h2>계정 연결 삭제</h2><p className="muted">이 계정의 수집과 발송을 중지합니다. 기존 메일 데이터는 유지됩니다.</p><Button variant="destructive" disabled={busy} onClick={() => run('delete')}>계정 연결 삭제</Button></Panel>
    </>}
  </div>;
}
