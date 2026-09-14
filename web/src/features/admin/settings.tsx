import { useState, type FormEvent } from 'react';
import { toast } from 'sonner';
import { api } from '@/api/client';
import { Badge, Button, ErrorState, Input, Loading, Panel, Textarea } from '@/components/ui';
import { Field, useResource, type Settings } from './shared';

interface SettingField { key: string; label: string; type?: 'number' | 'checkbox' | 'textarea' | 'json' | 'url'; hint?: string; required?: boolean; options?: [string, string][]; writeOnly?: boolean }
interface SecretField { label: string; input: string; reference: string; inValues?: boolean }
interface TestResult { ok: boolean; message: string; model?: string; latency_ms?: number; ai_embed_ok?: boolean; vector_store_ok?: boolean; ai_embed_model?: string; vector_store_provider?: string }

function SettingsEditor({ title, description, fields, endpoint = '/api/admin/settings', secret, tests = [] }: { title: string; description?: string; fields: SettingField[]; endpoint?: string; secret?: SecretField; tests?: { label: string; path: string }[] }) {
  const { data: settings, error, setError, reload } = useResource<Settings>('/api/admin/settings');
  const [busy, setBusy] = useState(false);
  const [test, setTest] = useState<TestResult>();
  async function submit(e: FormEvent<HTMLFormElement>) {
    e.preventDefault();
    const form = e.currentTarget, data = new FormData(form), values: Settings = {};
    const newSecret = String(data.get('new_secret') || '');
    const clearSecret = data.get('clear_secret') === 'on';
    setError(undefined);
    try {
      if (newSecret && clearSecret) throw new Error('새 비밀값 입력과 기존 값 삭제를 동시에 선택할 수 없습니다.');
      for (const field of fields) {
        const value = field.type === 'checkbox' ? String(data.get(field.key) === 'on') : String(data.get(field.key) || '');
        if (field.writeOnly && !value && data.get(`${field.key}.clear`) !== 'on') continue;
        if (field.type === 'json' && value.trim()) JSON.parse(value);
        values[field.key] = value;
      }
      if (secret && clearSecret) {
        if (!window.confirm(`${secret.label}을 삭제하시겠습니까? 이 값을 사용하는 연결이 중단될 수 있습니다.`)) return;
        values[secret.reference] = '';
      }
      const body: Record<string, unknown> = { values };
      if (secret && newSecret) {
        if (secret.inValues) values[secret.input] = newSecret;
        else body[secret.input] = newSecret;
      }
      setBusy(true);
      await api<Settings>(endpoint, { method: endpoint === '/api/admin/ai' ? 'PUT' : 'PATCH', body });
      toast.success('설정을 저장했습니다.'); setTest(undefined); reload();
    } catch (err) { setError(err instanceof SyntaxError ? new Error('JSON 형식이 올바르지 않습니다. 입력한 설정을 확인하세요.') : err); }
    finally {
      for (const name of ['new_secret', ...fields.filter(field => field.writeOnly).map(field => field.key)]) { const input = form.elements.namedItem(name); if (input instanceof HTMLInputElement || input instanceof HTMLTextAreaElement) input.value = ''; }
      const clear = form.elements.namedItem('clear_secret'); if (clear instanceof HTMLInputElement) clear.checked = false;
      setBusy(false);
    }
  }
  async function runTest(path: string) {
    setBusy(true); setError(undefined); setTest(undefined);
    try { setTest(await api<TestResult>(path, { method: 'POST' })); } catch (err) { setError(err); } finally { setBusy(false); }
  }
  return <Panel><h2>{title}</h2>{description && <p className="muted">{description}</p>}{!!error && <ErrorState error={error} retry={reload} />}
    {!settings ? !error && <Loading /> : <form className="stack" onSubmit={submit} key={JSON.stringify(fields.filter(field => !field.writeOnly).map(field => settings[field.key]))}>
      <fieldset disabled={busy} className="stack" style={{ border: 0, padding: 0 }}><div className="grid">
        {fields.map(field => field.type === 'checkbox' ? <label className="row" key={field.key}><input type="checkbox" name={field.key} defaultChecked={settings[field.key] === 'true'} /><span>{field.label}{field.hint && <small className="muted" style={{ display: 'block' }}>{field.hint}</small>}</span></label> : <Field key={field.key} label={field.label} hint={field.hint}>
          {field.options ? <select className="input" name={field.key} defaultValue={settings[field.key] || field.options[0][0]}>{field.options.map(([value, label]) => <option key={value} value={value}>{label}</option>)}</select> : field.type === 'textarea' || field.type === 'json' ? <Textarea name={field.key} defaultValue={field.writeOnly ? '' : settings[field.key] || ''} rows={field.type === 'json' ? 5 : 4} required={field.required} autoComplete={field.writeOnly ? 'off' : undefined} spellCheck={field.type !== 'json'} placeholder={field.writeOnly ? '변경할 때만 새 값을 입력하세요.' : undefined} /> : <Input name={field.key} type={field.type === 'number' ? 'number' : field.type === 'url' ? 'url' : 'text'} min={field.type === 'number' ? 0 : undefined} defaultValue={settings[field.key] || ''} required={field.required} />}
        </Field>)}
      </div>
      {fields.filter(field => field.writeOnly).map(field => <label className="row" key={field.key}><input type="checkbox" name={`${field.key}.clear`} />저장된 {field.label} 비우기</label>)}
      {secret && <><Field label={secret.label} hint={settings[secret.reference] ? '등록된 값이 있습니다. 변경할 때만 입력하세요. 저장된 값은 다시 조회할 수 없습니다.' : '등록된 값이 없습니다. 필요한 경우 입력하세요.'}><Input name="new_secret" type="password" autoComplete="new-password" /></Field>{settings[secret.reference] && <label className="row"><input name="clear_secret" type="checkbox" />저장된 {secret.label} 삭제</label>}</>}
      <div className="row"><Button type="submit" disabled={busy}>{busy ? '처리 중…' : '설정 저장'}</Button>{tests.map(item => <Button key={item.path} type="button" variant="outline" disabled={busy} onClick={() => runTest(item.path)}>{item.label}</Button>)}</div>
      {tests.length > 0 && <p className="muted">연결 테스트는 서버에 저장된 설정으로 실행합니다. 변경한 값은 먼저 저장하세요.</p>}
      </fieldset>
    </form>}
    {test && <section aria-live="polite" className="stack"><div className="row"><Badge variant={test.ok ? 'secondary' : 'destructive'}>{test.ok ? '연결 정상' : '연결 확인 필요'}</Badge>{test.model && <span>{test.model}</span>}{test.latency_ms !== undefined && <span>{test.latency_ms} ms</span>}</div><p>{test.message}</p>{test.ai_embed_ok !== undefined && <p>임베딩: {test.ai_embed_ok ? '정상' : '실패'} · 벡터 저장소: {test.vector_store_ok ? '정상' : '실패'}</p>}</section>}
  </Panel>;
}

const oidcFields: SettingField[] = [
  { key: 'auth.oidc.issuer', label: 'Keycloak Issuer URL', type: 'url', hint: '예: https://sso.corp.local/realms/corp' },
  { key: 'auth.oidc.client_id', label: 'Client ID' }, { key: 'auth.oidc.redirect_url', label: 'Redirect URL', type: 'url', hint: '기존 Postra OIDC 콜백 주소를 사용합니다.' },
  { key: 'auth.oidc.admin_group', label: '관리자 그룹' }, { key: 'auth.session_hours', label: '세션 유지 시간', type: 'number' },
  { key: 'auth.oidc.auto_provision', label: 'SSO 최초 로그인 시 Postra 사용자 생성', type: 'checkbox' }, { key: 'auth.oidc.auto_login', label: 'Keycloak 세션이 있으면 자동 로그인', type: 'checkbox' },
];
const aiFields: SettingField[] = [
  { key: 'ai.base_url', label: 'AI API Base URL', type: 'url', required: true, hint: '사내 OpenAI 호환 서버 주소도 사용할 수 있습니다.' },
  { key: 'ai.model', label: 'Chat 모델', required: true }, { key: 'ai.embed_model', label: '임베딩 모델' }, { key: 'ai.embed_base_url', label: '임베딩 전용 Base URL', type: 'url', hint: '비워 두면 기본 AI 서버를 사용합니다.' },
  { key: 'ai.timeout_sec', label: '응답 대기 시간(초)', type: 'number' }, { key: 'ai.max_tokens', label: '최대 출력 토큰', type: 'number' },
  { key: 'ai.stream', label: '스트리밍 응답 사용', type: 'checkbox' }, { key: 'ai.allow_external', label: '외부 AI 서비스로 데이터 전송 허용', type: 'checkbox' }, { key: 'ai.mask_external_pii', label: '외부 AI 요청의 개인정보 마스킹', type: 'checkbox' },
  { key: 'ai.extra_headers', label: '추가 API 헤더(JSON)', type: 'json', writeOnly: true, hint: '새 값을 입력하면 전체 헤더를 교체합니다. 저장된 인증 헤더는 화면에 재노출하지 않습니다.' },
  { key: 'ai.task_models', label: '작업별 모델 설정(JSON)', type: 'json', hint: '필요한 경우에만 작업명별 model, base_url, api_key_ref 등을 설정합니다.' },
];
const securityFields: SettingField[] = [
  { key: 'security.allow_private_hosts', label: '사설망 메일 서버 허용', type: 'checkbox' }, { key: 'security.allow_insecure_mail', label: '평문 연결·무인증 SMTP·인증서 검증 생략 허용', type: 'checkbox' }, { key: 'security.encrypt_at_rest', label: '저장 데이터 암호화', type: 'checkbox' },
  { key: 'send.dlp_policy', label: '외부 발송 민감정보 정책', options: [['warn', '경고'], ['block', '차단'], ['off', '사용 안 함']] },
  { key: 'send.dlp_keywords', label: '민감정보 키워드', hint: '쉼표로 구분합니다.' }, { key: 'send.warn_recipients', label: '수신자 수 경고 기준', type: 'number' },
  { key: 'send.max_per_minute', label: '계정별 분당 발송 한도', type: 'number' }, { key: 'send.max_per_hour', label: '계정별 시간당 발송 한도', type: 'number' },
  { key: 'send.max_retries', label: '발송 최대 시도 횟수', type: 'number' }, { key: 'send.retry_base_seconds', label: '재시도 기본 간격(초)', type: 'number' }, { key: 'send.retry_max_seconds', label: '재시도 최대 간격(초)', type: 'number' },
  { key: 'attachments.block_extensions', label: '차단할 첨부 확장자', hint: '쉼표로 구분합니다.' }, { key: 'attachments.quarantine_extensions', label: '격리할 첨부 확장자', hint: '쉼표로 구분합니다.' },
  { key: 'attachments.archive_max_entries', label: '압축파일 최대 항목 수', type: 'number' }, { key: 'attachments.archive_max_total_bytes', label: '압축 해제 최대 크기(bytes)', type: 'number' }, { key: 'attachments.archive_max_ratio', label: '압축률 제한', type: 'number' },
];
const syncFields: SettingField[] = [
  { key: 'sync.auto_sync_minutes', label: '자동 수집 간격(분)', type: 'number' }, { key: 'sync.initial_window_days', label: '최초 수집 기간(일)', type: 'number' },
  { key: 'sync.max_message_bytes', label: '메일 한 건 최대 크기(bytes)', type: 'number' }, { key: 'sync.max_per_sync', label: '한 번에 수집할 최대 메일 수', type: 'number' },
  { key: 'sync.connect_timeout_sec', label: '서버 연결 제한 시간(초)', type: 'number' }, { key: 'sync.command_timeout_sec', label: '명령 응답 제한 시간(초)', type: 'number' },
  { key: 'compose.writing_guide', label: '조직 메일 작성 가이드', type: 'textarea' }, { key: 'compose.banned_phrases', label: '작성 시 경고할 표현', hint: '쉼표로 구분합니다.' },
];

export function OIDCSettingsPanel() { return <SettingsEditor title="Keycloak SSO 설정" description="SSO 인증 설정과 Postra 사용자 자동 생성을 관리합니다. 메일 계정 자동 연결은 메일 프로비저닝에서 별도로 설정하세요." fields={oidcFields} secret={{ label: 'OIDC Client Secret', input: 'oidc_client_secret', reference: 'auth.oidc.secret_ref' }} />; }
export function AISettingsPanel() { return <div className="stack"><SettingsEditor title="AI 연결 설정" description="API 키는 변경할 때만 입력합니다. 저장 후 서버에서 안전하게 보관하며 재기동 후에도 저장된 설정을 사용합니다." fields={aiFields} endpoint="/api/admin/ai" secret={{ label: 'AI API Key', input: 'api_key', reference: 'ai.api_key_ref' }} tests={[{ label: '저장된 Chat 연결 테스트', path: '/api/admin/ai/test' }, { label: '임베딩·벡터 연결 테스트', path: '/api/admin/vector/test' }]} /><SettingsEditor title="벡터 저장소" description="벡터 저장소 설정 변경은 재기동 후 적용됩니다." fields={[
  { key: 'vector.provider', label: '저장소', options: [['', '자동 선택'], ['sqlite', 'SQLite'], ['postgres', 'PostgreSQL'], ['milvus', 'Milvus']] }, { key: 'vector.milvus_url', label: 'Milvus URL', type: 'url' }, { key: 'vector.milvus_collection', label: 'Milvus 컬렉션' },
]} secret={{ label: 'Milvus Token', input: 'vector.milvus_token', reference: 'vector.milvus_token_ref', inValues: true }} /></div>; }
export function SecuritySettingsPanel() { return <SettingsEditor title="보안·발송 정책" description="운영 정책을 저장합니다. 메일·첨부파일 정책의 변경은 서버 재기동 후 적용됩니다." fields={securityFields} />; }
export function GeneralSettingsPanel() { return <div className="stack"><SettingsEditor title="동기화·작성 정책" description="변경한 수집·작성 정책은 서버 재기동 후 적용됩니다." fields={syncFields} /><SettingsEditor title="MCP 도구 접근 정책" description="역할별 도구 접근 정책을 JSON으로 관리합니다. 비워 두면 기본 정책을 적용합니다." fields={[{ key: 'mcp.policy', label: 'MCP 정책(JSON)', type: 'json' }]} /></div>; }
