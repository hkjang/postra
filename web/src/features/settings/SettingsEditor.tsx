import {useContext, useEffect, useMemo, useState} from 'react'
import {Link, UNSAFE_DataRouterContext, useBlocker} from 'react-router-dom'
import {useQuery, useQueryClient} from '@tanstack/react-query'
import * as Dialog from '@radix-ui/react-dialog'
import {Info, LockKeyhole, Save, Search, X} from 'lucide-react'
import {toast} from 'sonner'
import {api} from '@/api/client'
import type {SettingsPatch} from '@/api/contracts.generated'
import {Badge, Button, EmptyState, ErrorState, Input, Loading} from '@/components/ui'
import type {SettingField, SettingsView} from './preferences'
import {ConnectionDiagnostic, useConnectionProbe} from './ConnectionDiagnostic'
import './settings.css'

const sources: Record<string, string> = {default: '프로그램 기본값', configuration: '배포 설정 파일', environment: '환경변수 초기값', admin: '관리자 설정', user: '사용자 설정', account: '메일 계정 설정', policy: '관리자 강제 정책', admin_policy: '관리자 강제 정책', fixed_policy: '항상 적용되는 고정 정책'}
const optionLabels: Record<string, string> = {true: '사용', false: '사용 안 함', system: '시스템 테마', light: '라이트', dark: '다크', comfortable: '편안하게', compact: '간결하게', right: '오른쪽', bottom: '아래', hidden: '숨김', auto: '자동 서식', text: '일반 텍스트', html: 'HTML', markdown: 'Markdown', ko: '한국어', en: 'English', relative: '상대 시간', absolute: '날짜 및 시간', iso: 'ISO 날짜', full: '항상 전체 서명', smart: '새 메일 전체 · 첫 회신 간결 · 후속 회신 생략', none: '사용 안 함'}
export const adminCategory = (field: SettingField) => ({appearance: 'general', personal_mail: 'mail', compose: 'mail', account: 'mail', personal_ai: 'ai', personal_notifications: 'notifications', vector: 'search'}[field.category] || field.category)
const secretLabel = (field: SettingField) => field.registered ? '등록됨 · 새 값을 입력하면 교체' : '미등록 · 새 값 입력'
const tasks = [['summarize', '메일 요약'], ['compose', '메일 작성'], ['classify', '메일 분류'], ['qa', 'Q&A'], ['rewrite', '다시 쓰기'], ['digest', '브리핑']]

function NavigationGuard({dirty, onDiscard}: {dirty: boolean; onDiscard: () => void}) {
  const blocker = useBlocker(({currentLocation, nextLocation}) => dirty && currentLocation.pathname + currentLocation.search !== nextLocation.pathname + nextLocation.search)
  return <Dialog.Root open={blocker.state === 'blocked'} onOpenChange={open => {if (!open && blocker.state === 'blocked') blocker.reset()}}><Dialog.Portal><Dialog.Overlay className="dialog-overlay"/><Dialog.Content className="settings-confirm"><Dialog.Title>저장하지 않은 설정이 있습니다</Dialog.Title><Dialog.Description>이동하면 입력 중인 값과 비밀값이 지워집니다. 설정을 저장하려면 이 화면에 머무르세요.</Dialog.Description><div className="row"><Button variant="outline" onClick={() => {if (blocker.state === 'blocked') blocker.reset()}}>계속 편집</Button><Button onClick={() => {if (blocker.state === 'blocked') {onDiscard(); blocker.proceed()}}}>변경 버리고 이동</Button></div></Dialog.Content></Dialog.Portal></Dialog.Root>
}

function TaskModels({value, onChange, disabled}: {value: string; onChange: (value: string) => void; disabled: boolean}) {
  type Route = {model?: string; base_url?: string; max_tokens?: number; api_key_ref?: string}
  let routes: Record<string, Route> = {}
  try {routes = JSON.parse(value || '{}') || {}} catch { /* preserve invalid input for correction below */ }
  const change = (task: string, model: string) => {
    const next = {...routes, [task]: {...routes[task], model}}
    if (!model && !next[task].base_url && !next[task].api_key_ref && !next[task].max_tokens) delete next[task]
    onChange(JSON.stringify(next))
  }
  return <div className="stack"><div className="task-model-grid">{tasks.map(([key, label]) => <label key={key} className="field"><span>{label}</span><Input disabled={disabled} value={routes[key]?.model || ''} placeholder="기본 Chat Model 상속" onChange={event => change(key, event.target.value)}/></label>)}</div><details><summary>전용 Endpoint·토큰 한도 고급 설정</summary><textarea aria-label="작업별 모델 JSON" className="input" rows={6} value={value} disabled={disabled} onChange={event => onChange(event.target.value)}/><small className="muted">작업별 model, base_url, max_tokens, api_key_ref만 지정합니다. 비밀번호나 API Key 원문을 넣지 마세요. 임베딩은 전용 Embedding Model 항목을 사용합니다.</small></details></div>
}

export function SettingsEditor({admin = false, accountID, category, query: externalQuery}: {admin?: boolean; accountID?: string; category?: string; query?: string}) {
  const dataRouter = useContext(UNSAFE_DataRouterContext)
  const cache = useQueryClient()
  const path = admin ? '/api/admin/configuration' : accountID ? `/api/accounts/${encodeURIComponent(accountID)}/preferences` : '/api/preferences'
  const key = admin ? ['configuration'] : ['preferences', accountID || 'user']
  const current = useQuery({queryKey: key, queryFn: ({signal}) => api<SettingsView>(path, {signal})})
  const accounts = useQuery({queryKey: ['accounts'], queryFn: () => api<{id: string; name: string; email: string}[]>('/api/accounts')})
  const signatures = useQuery({queryKey: ['signatures'], queryFn: () => api<{id: string; name: string}[]>('/api/signatures')})
  const [values, setValues] = useState<Record<string, string>>({})
  const [secrets, setSecrets] = useState<Record<string, string>>({})
  const [locks, setLocks] = useState<Record<string, boolean>>({})
  const [reset, setReset] = useState<string[]>([])
  const [query, setQuery] = useState('')
  const [confirm, setConfirm] = useState(false)
  const [busy, setBusy] = useState(false)
  const [probeTask, setProbeTask] = useState('')
  const probe = useConnectionProbe(`${path}:${category || ''}:${current.data?.revision || ''}`)
  const [error, setError] = useState<unknown>()
  const dirty = Object.keys(values).length + Object.values(secrets).filter(Boolean).length + Object.keys(locks).length + reset.length
  // Do not erase a pending form on a background refresh. The revision check
  // will reject stale writes rather than overwrite another operator's work.
  const [revision, setRevision] = useState('')
  useEffect(() => {if (!dirty && current.data) setRevision(current.data.revision)}, [current.data, dirty])
  useEffect(() => {
    if (!dirty) return
    const leave = (event: BeforeUnloadEvent) => {event.preventDefault(); event.returnValue = ''}
    window.addEventListener('beforeunload', leave)
    return () => window.removeEventListener('beforeunload', leave)
  }, [dirty])
  const selected = useMemo(() => {
    const search = (externalQuery ?? query).trim().toLowerCase()
    return (current.data?.fields ?? []).filter(field => field.key !== 'ai.extra_headers' &&
      (!category || search || adminCategory(field) === category) &&
      (!search || [field.key, field.label, field.help, ...(field.environment || [])].join(' ').toLowerCase().includes(search)))
  }, [current.data, category, query, externalQuery])
  const edit = (field: SettingField, value: string) => {
    probe.clear()
    setReset(old => old.filter(key => key !== field.key))
    if (field.secret) setSecrets(old => ({...old, [field.key]: value}))
    else setValues(old => {const next = {...old}; if (field.value === value) delete next[field.key]; else next[field.key] = value; return next})
  }
  const discard = () => {setValues({}); setSecrets({}); setLocks({}); setReset([]); setConfirm(false); setError(undefined); probe.clear()}
  const changedFields = current.data?.fields?.filter(field => field.key in values || secrets[field.key] || field.key in locks || reset.includes(field.key)) || []
  function testCandidate(target: string) {
    setError(undefined)
    void probe.run('/api/admin/configuration/test', {target, values, secrets, ...(['ai', 'ai_models'].includes(target) && probeTask ? {task: probeTask} : {})}, target.endsWith('_models'))
  }
  async function save() {
    probe.clear(); setBusy(true); setError(undefined)
    try {
      const saved = await api<SettingsView>(path, {method: 'PATCH', body: {values, secrets, locks, reset, revision} satisfies SettingsPatch})
      cache.setQueryData(key, saved)
      setValues({}); setLocks({}); setReset([]); setConfirm(false); setRevision(saved.revision)
      await cache.invalidateQueries({queryKey: ['preferences']})
      await cache.invalidateQueries({queryKey: ['operations']})
      toast.success('설정을 저장했습니다. 즉시 적용 항목은 다음 작업부터 반영됩니다.')
    } catch (err) {setError(err)}
    finally {setSecrets({}); setBusy(false)}
  }
  if (current.isPending) return <Loading/>
  if (current.error) return <ErrorState error={current.error} retry={() => current.refetch()}/>
  return <div className="settings-editor stack">
    {dataRouter && <NavigationGuard dirty={dirty > 0} onDiscard={discard}/>}
    {externalQuery === undefined && <label className="settings-search"><Search size={17}/><Input aria-label="설정 검색" placeholder={admin ? '설정 이름 또는 POSTRA_AI_BASE_URL 검색' : '개인 설정 검색'} value={query} onChange={event => setQuery(event.target.value)}/></label>}
    {error != null && <ErrorState error={error}/>}
    {admin && ['ai', 'search', 'auth', 'storage'].includes(category || '') && <div className="stack">
      {['ai', 'search'].includes(category || '') && <>
        <label className="field probe-task"><span>Chat 진단 작업</span><select className="input" aria-label="Chat 진단 작업" disabled={busy} value={probeTask} onChange={event => {probe.clear(); setProbeTask(event.target.value)}}><option value="">기본 Chat Model</option>{tasks.map(([key, label]) => <option key={key} value={key}>{label}</option>)}</select></label>
        <div className="row settings-probe-actions"><Button variant="outline" disabled={busy || probe.busy} onClick={() => testCandidate('ai_models')}>저장 전 Chat 모델 한도 조회</Button><Button variant="outline" disabled={busy || probe.busy} onClick={() => testCandidate('embedding_models')}>저장 전 Embedding 모델 한도 조회</Button></div>
        <small className="muted">모델 한도 조회는 /models 메타데이터만 읽으며 Chat 생성·임베딩을 실행하지 않습니다.</small>
      </>}
      <div className="row settings-probe-actions"><Button variant="outline" disabled={busy || probe.busy} onClick={() => testCandidate(category === 'auth' ? 'oidc' : category === 'storage' ? 'database' : 'ai')}>{category === 'auth' ? '저장 전 OIDC Discovery 확인' : category === 'storage' ? '현재 Database 연결 확인' : '저장 전 AI 연결 확인'}</Button>{['ai', 'search'].includes(category || '') && <Button variant="outline" disabled={busy || probe.busy} onClick={() => testCandidate('embedding')}>저장 전 Embedding 확인</Button>}</div>
      <small className="muted">입력한 변경값으로 시험합니다. 설정·비밀값을 저장하거나 운영 연결을 교체하지 않습니다.</small>
      {probe.busy && <p role="status">확인 중…</p>}{probe.result && <ConnectionDiagnostic result={probe.result.data} metadataOnly={probe.result.metadataOnly} showModelLimits={['ai', 'search'].includes(category || '')}/>} {probe.error != null && <ErrorState error={probe.error}/>}
    </div>}
    {selected.length === 0 && <EmptyState title="검색된 설정이 없습니다"/>}
    <div className="settings-fields">{selected.map(field => {
      const value = field.secret ? secrets[field.key] || '' : values[field.key] ?? field.value
      const disabled = busy || ['deployment', 'fixed'].includes(field.apply) || (!admin && field.locked)
      const id = `setting-${field.key}`
      return <section className="setting-row" key={field.key}>
        <div className="setting-description"><label htmlFor={id}>{field.label}</label><div className="row">{field.apply === 'restart' && <Badge variant="outline">재시작 필요</Badge>}{field.apply === 'deployment' && <Badge variant="outline">배포 초기값</Badge>}{field.apply === 'fixed' && <Badge variant="outline">항상 적용</Badge>}{field.scope !== 'admin' && admin && <Badge variant="secondary">개인 설정 기본값</Badge>}{field.locked && !admin && <Badge><LockKeyhole size={12}/>조직 정책</Badge>}{field.pending_restart && <Badge variant="destructive">적용 대기</Badge>}</div>{field.help && <small className="muted">{field.help}</small>}
          <details className="setting-details"><summary><Info size={12}/>값 출처 및 환경변수</summary><dl><dt>출처</dt><dd>{sources[field.source] || field.source}</dd><dt>설정 키</dt><dd><code>{field.key}</code></dd>{field.environment?.length ? <><dt>초기 환경변수</dt><dd>{field.environment.map(name => <code key={name}>{name}</code>)}</dd></> : null}{field.pending_restart && !field.secret && <><dt>현재 실행 값</dt><dd>{field.active_value}</dd></>}</dl></details>
        </div>
        <div className="setting-control">{field.key === 'ai.task_models' ? <TaskModels value={value} onChange={value => edit(field, value)} disabled={disabled}/> : field.type === 'bool' ? <label className="row setting-toggle"><input id={id} type="checkbox" disabled={disabled} checked={value === 'true'} onChange={event => edit(field, String(event.target.checked))}/>{value === 'true' ? '사용' : '사용 안 함'}</label> : field.type === 'enum' ? <select className="input" id={id} disabled={disabled} value={value} onChange={event => edit(field, event.target.value)}>{field.options?.map(option => <option key={option} value={option}>{optionLabels[option] || option || '기본값'}</option>)}</select> : ['account', 'signature'].includes(field.type) ? <select id={id} className="input" disabled={disabled} value={value} onChange={event => edit(field, event.target.value)}><option value="">자동 선택 / 없음</option>{(field.type === 'account' ? accounts.data || [] : signatures.data || []).map(item => <option key={item.id} value={item.id}>{item.name || item.id}</option>)}</select> : field.type === 'json' || field.type === 'text' ? <textarea id={id} className="input" rows={4} disabled={disabled} value={value} onChange={event => edit(field, event.target.value)}/> : <Input id={id} type={field.secret ? 'password' : ['int', 'number'].includes(field.type) ? 'number' : field.type === 'url' ? 'url' : 'text'} step={field.type === 'number' ? 'any' : undefined} autoComplete={field.secret ? 'new-password' : 'off'} disabled={disabled} placeholder={field.secret ? secretLabel(field) : undefined} value={value} onChange={event => edit(field, event.target.value)}/>}
          {field.secret && <small className="muted">{field.registered ? '등록됨' : '미등록'} · 원문은 재조회할 수 없습니다. 빈 값은 유지합니다.</small>}
          {admin && field.lockable && <label className="row policy-lock"><input type="checkbox" checked={locks[field.key] ?? field.locked} disabled={busy} onChange={event => {probe.clear(); setLocks(old => {const next = {...old}; if (event.target.checked === field.locked) delete next[field.key]; else next[field.key] = event.target.checked; return next})}}/><LockKeyhole size={13}/>관리자 값 강제 적용</label>}
          {!admin && !field.locked && ['user', 'account'].includes(field.source) && <Button variant="ghost" size="sm" disabled={busy || reset.includes(field.key)} onClick={() => {setReset(old => [...old, field.key]); setValues(old => {const next = {...old}; delete next[field.key]; return next})}}>{reset.includes(field.key) ? '상속값으로 복원 예정' : '기본값 상속으로 복원'}</Button>}
          {field.type === 'signature' && <Link to="/settings/signatures">서명 여러 개 관리</Link>}
        </div>
      </section>
    })}</div>
    <footer className="settings-savebar"><span>{dirty ? `${changedFields.length}개 항목 변경 · 아직 저장되지 않음` : '모든 변경사항이 저장되었습니다.'}</span><div className="row">{dirty > 0 && <Button variant="ghost" disabled={busy} onClick={discard}>변경 취소</Button>}<Button disabled={!dirty || busy} onClick={() => setConfirm(true)}><Save size={15}/>변경 확인</Button></div></footer>
    <Dialog.Root open={confirm} onOpenChange={open => {if (!busy) setConfirm(open)}}><Dialog.Portal><Dialog.Overlay className="dialog-overlay"/><Dialog.Content className="settings-confirm"><div className="row between"><Dialog.Title>저장할 변경사항</Dialog.Title><Dialog.Close asChild><Button variant="ghost" size="icon" aria-label="닫기"><X size={18}/></Button></Dialog.Close></div><Dialog.Description>조직 정책은 개인 설정보다 우선합니다. 재시작 항목은 기존 연결을 유지한 채 다음 실행에 적용됩니다.</Dialog.Description><ul>{changedFields.map(field => <li key={field.key}><strong>{field.label}</strong><p>{field.secret ? '비밀값 등록·교체 (원문 표시 안 함)' : reset.includes(field.key) ? '기본값 상속으로 복원' : `${field.value || '(비어 있음)'} → ${values[field.key] ?? field.value}`}</p>{field.key in locks && <p>{locks[field.key] ? '사용자 변경 금지' : '사용자 변경 허용'}</p>}{field.apply === 'restart' && <Badge variant="outline">재시작 필요</Badge>}</li>)}</ul>{error != null && <ErrorState error={error}/>}<Button disabled={busy} onClick={save}>{busy ? '저장 중…' : '확인 후 저장'}</Button></Dialog.Content></Dialog.Portal></Dialog.Root>
  </div>
}
