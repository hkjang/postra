import { useEffect, useRef, useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Link, useBlocker } from 'react-router-dom'
import * as Dialog from '@radix-ui/react-dialog'
import { Plus, Save, Trash2 } from 'lucide-react'
import { toast } from 'sonner'
import { api } from '@/api/client'
import { Button, EmptyState, ErrorState, Input, Loading, PageHeader } from '@/components/ui'
import { MailBody } from '../messages/MailBody'
import type { Account } from '../messages/types'
import { RichEditor } from './RichEditor'
import type { MailSignature } from './rendering'
import './compose.css'

const empty = { name: '', account_id: '', body_html: '', display_name: '', title: '', department: '', company: '', phone: '', email: '', logo_url: '' }
export function SignaturesPage() {
  const cache = useQueryClient()
  const [selected, setSelected] = useState('')
  const [form, setForm] = useState(empty)
  const [dirty, setDirty] = useState(false)
  const dirtyRef = useRef(false)
  const logoutWasDirty = useRef(false)
  const [deleting, setDeleting] = useState<MailSignature>()
  const signatures = useQuery({ queryKey: ['signatures'], queryFn: ({ signal }) => api<MailSignature[]>('/api/signatures', { signal }) })
  const accounts = useQuery({ queryKey: ['accounts'], queryFn: ({ signal }) => api<Account[]>('/api/accounts', { signal }) })
  const blocker = useBlocker(() => dirtyRef.current)
  function changed(value: boolean) { dirtyRef.current = value; setDirty(value) }
  function edit(value: Partial<typeof empty>) { setForm(current => ({ ...current, ...value })); changed(true) }
  function load(signature?: MailSignature) {
    if (dirtyRef.current && !window.confirm('저장하지 않은 서명 변경을 버리고 이동하시겠습니까?')) return
    setSelected(signature?.id || ''); setForm(signature ? { name: signature.name, account_id: signature.account_id || '', body_html: signature.body_html || '', display_name: signature.display_name || '', title: signature.title || '', department: signature.department || '', company: signature.company || '', phone: signature.phone || '', email: signature.email || '', logo_url: signature.logo_url || '' } : empty); changed(false)
  }
  const save = useMutation({ mutationFn: () => api<MailSignature>(selected ? `/api/signatures/${encodeURIComponent(selected)}` : '/api/signatures', { method: selected ? 'PUT' : 'POST', body: { ...form, format: 'html' } }), onSuccess: value => {
    changed(false); setSelected(value.id); setForm(current => ({ ...current, body_html: value.body_html || '' })); cache.invalidateQueries({ queryKey: ['signatures'] }); toast.success('서명을 저장했습니다.')
  } })
  const remove = useMutation({ mutationFn: (id: string) => api(`/api/signatures/${encodeURIComponent(id)}`, { method: 'DELETE' }), onSuccess: (_, id) => {
    if (selected === id) { setSelected(''); setForm(empty); changed(false) }
    setDeleting(undefined); cache.invalidateQueries({ queryKey: ['signatures'] }); cache.invalidateQueries({ queryKey: ['preferences'] }); toast.success('서명을 삭제했습니다. 기본 서명이었다면 개인 설정에서 다시 선택해 주세요.')
  } })
  const makeDefault = useMutation({ mutationFn: (signature: MailSignature) => api(signature.account_id ? `/api/accounts/${encodeURIComponent(signature.account_id)}/preferences` : '/api/preferences', { method: 'PATCH', body: { values: { 'compose.signature_id': signature.id, 'compose.use_signature': 'true' } } }), onSuccess: () => { cache.invalidateQueries({ queryKey: ['preferences'] }); toast.success('기본 서명을 지정했습니다.') } })
  const busy = save.isPending || remove.isPending || makeDefault.isPending
  useEffect(() => {
    const unload = (event: BeforeUnloadEvent) => { if (dirtyRef.current) { event.preventDefault(); event.returnValue = '' } }
    const logout = (event: Event) => {
      if (busy) { event.preventDefault(); toast.error('서명 저장이 끝난 뒤 로그아웃해 주세요.'); return }
      if (!dirtyRef.current) return
      if (!window.confirm('저장하지 않은 서명 변경이 사라집니다. 로그아웃하시겠습니까?')) { event.preventDefault(); return }
      logoutWasDirty.current = true; changed(false)
    }
    const failed = () => { if (logoutWasDirty.current) { logoutWasDirty.current = false; changed(true) } }
    window.addEventListener('beforeunload', unload); window.addEventListener('postra:before-logout', logout); window.addEventListener('postra:logout-failed', failed)
    return () => { window.removeEventListener('beforeunload', unload); window.removeEventListener('postra:before-logout', logout); window.removeEventListener('postra:logout-failed', failed) }
  }, [busy])
  if (signatures.isPending || accounts.isPending) return <Loading />
  if (signatures.error) return <ErrorState error={signatures.error} retry={() => signatures.refetch()} />
  if (accounts.error) return <ErrorState error={accounts.error} retry={() => accounts.refetch()} />
  return <div className="page signature-page">
    <PageHeader title="내 메일 서명" description="서명은 본인만 조회·수정할 수 있습니다. 계정별 서명과 기본 서명을 관리하세요." actions={<Button asChild variant="outline"><Link to="/settings">개인 설정</Link></Button>} />
    {(save.error || remove.error || makeDefault.error) && <ErrorState error={save.error || remove.error || makeDefault.error} />}
    <div className="signature-layout"><aside className="signature-list"><Button variant="outline" disabled={busy} onClick={() => load()}><Plus size={16} /> 새 서명</Button>{signatures.data?.length ? signatures.data.map(signature => <div key={signature.id} className="signature-item"><Button variant={selected === signature.id ? 'secondary' : 'ghost'} disabled={busy} onClick={() => load(signature)}>{signature.name}</Button><Button size="icon" variant="ghost" disabled={busy} aria-label={`${signature.name} 삭제`} onClick={() => setDeleting(signature)}><Trash2 size={15} /></Button><Button size="sm" variant="ghost" disabled={busy} onClick={() => makeDefault.mutate(signature)}>기본 서명으로</Button></div>) : <EmptyState title="등록된 서명이 없습니다" description="이름·회사 등으로 기본 서명을 만들거나 직접 편집하세요." />}</aside>
    <form className="signature-editor" onSubmit={event => { event.preventDefault(); save.mutate() }}><fieldset disabled={busy}><div className="signature-identity"><label className="field">서명 이름<Input required maxLength={120} aria-label="서명 이름" value={form.name} onChange={event => edit({ name: event.target.value })} /></label><label className="field">사용 계정<select aria-label="서명 사용 계정" value={form.account_id} onChange={event => edit({ account_id: event.target.value })}><option value="">내 모든 메일 계정</option>{accounts.data?.map(account => <option key={account.id} value={account.id}>{account.email}</option>)}</select></label>{([['display_name', '표시 이름'], ['title', '직책'], ['department', '부서'], ['company', '회사'], ['phone', '전화'], ['email', '이메일']] as const).map(([key, label]) => <label key={key} className="field">{label}<Input aria-label={`서명 ${label}`} type={key === 'email' ? 'email' : 'text'} maxLength={200} value={form[key]} onChange={event => edit({ [key]: event.target.value })} /></label>)}</div>
    <p className="muted">본문을 비워 두면 위 정보로 서명을 생성합니다. 스마트 서명은 새 메일에 전체 서명, 첫 회신에는 이름·회사·이메일, 이후 같은 대화의 회신에는 서명을 생략합니다. 구조화된 정보가 없으면 첫 3줄을 사용합니다.</p>
    <RichEditor value={form.body_html} disabled={busy} onChange={(html, text) => edit({ body_html: text.trim() || html.includes('<img') ? html : '' })} />
    <label className="field">로고 URL (선택)<Input aria-label="서명 로고 URL" type="url" value={form.logo_url} onChange={event => edit({ logo_url: event.target.value })} placeholder="https://assets.corp.local/logo.png" /></label><p className="muted">로고는 본문을 비워 생성할 때 적용하며, 관리자의 발송 이미지 정책(차단·허용·프록시)을 따릅니다. 편집기와 로컬 미리보기는 외부 이미지를 요청하지 않습니다. 이미 저장한 서명 본문은 위 정보와 별도로 편집할 수 있습니다.</p>
    <div className="row"><Button type="submit" disabled={busy}><Save size={16} />{save.isPending ? '저장 중…' : '서명 저장'}</Button><span className="muted">{dirty ? '저장하지 않은 변경' : selected ? '저장됨' : '새 서명'}</span></div></fieldset>
    {!!form.body_html && <details><summary>안전한 서명 미리보기</summary><MailBody html={form.body_html} text="" title="메일 서명 미리보기" /></details>}</form></div>
    <Dialog.Root open={!!deleting} onOpenChange={open => { if (!open && !busy) setDeleting(undefined) }}><Dialog.Portal><Dialog.Overlay className="dialog-overlay"/><Dialog.Content className="signature-confirm" onEscapeKeyDown={event => { if (busy) event.preventDefault() }} onPointerDownOutside={event => event.preventDefault()}><Dialog.Title>“{deleting?.name}” 서명을 삭제하시겠습니까?</Dialog.Title><Dialog.Description>이미 저장되거나 발송된 메일 본문은 변경하지 않습니다.</Dialog.Description><div className="row"><Button variant="outline" disabled={busy} onClick={() => setDeleting(undefined)}>취소</Button><Button variant="destructive" disabled={busy} onClick={() => { if (deleting) remove.mutate(deleting.id) }}>서명 삭제 확인</Button></div></Dialog.Content></Dialog.Portal></Dialog.Root>
    <Dialog.Root open={blocker.state === 'blocked'} onOpenChange={open => { if (!open && blocker.state === 'blocked') blocker.reset() }}><Dialog.Portal><Dialog.Overlay className="dialog-overlay"/><Dialog.Content className="signature-confirm"><Dialog.Title>저장하지 않은 서명 변경이 있습니다</Dialog.Title><Dialog.Description>이동하면 저장하지 않은 본문과 정보가 사라집니다.</Dialog.Description><div className="row"><Button variant="outline" onClick={() => { if (blocker.state === 'blocked') blocker.reset() }}>계속 편집</Button><Button variant="destructive" disabled={busy} onClick={() => { if (blocker.state === 'blocked') { changed(false); blocker.proceed() } }}>변경을 버리고 이동</Button></div></Dialog.Content></Dialog.Portal></Dialog.Root>
  </div>
}
