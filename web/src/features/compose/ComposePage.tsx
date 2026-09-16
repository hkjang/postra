import { useEffect, useRef, useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Link, useBlocker, useNavigate, useParams, useSearchParams } from 'react-router-dom'
import * as Dialog from '@radix-ui/react-dialog'
import { CheckCircle2, Eye, FileText, Save, Send, ShieldCheck, Sparkles, Trash2, X } from 'lucide-react'
import { toast } from 'sonner'
import { api } from '@/api/client'
import type {CreateDraftInput, RenderMailInput, UpdateDraftInput} from '@/api/contracts.generated'
import {usePersonalPreferences} from '@/features/settings/preferences'
import { Badge, Button, EmptyState, ErrorState, Input, Loading, PageHeader, Textarea } from '@/components/ui'
import { addresses, type Account, type DraftView, type SendPreview } from '../messages/types'
import { RichEditor } from './RichEditor'
import { splitRecipients, textToHTML } from './utils'
import type { MailSignature, MailTemplate, PreferenceView, RenderedMail } from './rendering'
import { attachmentURL, DraftBody, fileBase64 } from './DraftBody'
import {defaultComposeAccount} from './default-account'
import {DraftSourcePanel} from './DraftSourcePanel'
import '../inbox/mail.css'
import './compose.css'

type Fields = { account: string; to: string; cc: string; bcc: string; subject: string; body: string; html: string }
type Approval = { token: string; expires: number }
type Outbound = { id: string; status: string; smtp_response?: string }
const empty: Fields = { account: '', to: '', cc: '', bcc: '', subject: '', body: '', html: '' }

export function ComposePage() {
  const { id } = useParams()
  const [params] = useSearchParams()
  const navigate = useNavigate()
  const cache = useQueryClient()
  const [fields, setFields] = useState<Fields>(empty)
  const [mode, setMode] = useState<'html' | 'source' | 'plain' | 'markdown' | 'auto'>('html')
  const [template, setTemplate] = useState('clean')
  const [signatureID, setSignatureID] = useState('')
  const [useSignature, setUseSignature] = useState(true)
  const [tone, setTone] = useState('professional')
  const [length, setLength] = useState('short')
  const [language, setLanguage] = useState('ko')
  const preferencesLoaded = useRef('')
  const [current, setCurrent] = useState<DraftView>()
  const [dirty, setDirtyState] = useState(false)
  const dirtyRef = useRef(false)
  const logoutWasDirty = useRef(false)
  function setDirty(value: boolean) { dirtyRef.current = value; setDirtyState(value) }
  const blocker = useBlocker(({ currentLocation, nextLocation }) => dirtyRef.current && currentLocation.pathname + currentLocation.search !== nextLocation.pathname + nextLocation.search)
  const [preview, setPreview] = useState<SendPreview>()
  const [approval, setApproval] = useState<Approval>()
  const [outbound, setOutbound] = useState<Outbound>()
  const [sendUncertain, setSendUncertain] = useState(false)
  const sendDispatched = useRef(false)
  const loaded = useRef('')
  const previousRoute = useRef(id)
  const accounts = useQuery({ queryKey: ['accounts'], queryFn: ({ signal }) => api<Account[]>('/api/accounts', { signal }) })
  const draft = useQuery({ queryKey: ['draft', id], queryFn: ({ signal }) => api<DraftView>(`/api/drafts/${encodeURIComponent(id!)}`, { signal }), enabled: !!id })
  const signatures = useQuery({ queryKey: ['signatures'], queryFn: ({ signal }) => api<MailSignature[]>('/api/signatures', { signal }) })
  const templates = useQuery({ queryKey: ['mail-templates'], queryFn: ({ signal }) => api<MailTemplate[]>('/api/mail/templates', { signal }), staleTime: Infinity })
  const preferences = useQuery({ queryKey: ['preferences', fields.account], queryFn: ({ signal }) => api<PreferenceView>(`/api/accounts/${encodeURIComponent(fields.account)}/preferences`, { signal }), enabled: !!fields.account })
  const personalPreferences = usePersonalPreferences()
  const preferredAccount = personalPreferences.value('mail.default_account_id')
  const explicitAccount = params.get('account') || params.get('account_id') || ''
  const locked = (key: string) => preferences.data?.fields?.find(field => field.key === key)?.locked || false
  useEffect(() => {
    if (!preferences.data || !fields.account || preferencesLoaded.current === fields.account) return
    preferencesLoaded.current = fields.account
    const value = (key: string, fallback: string) => preferences.data.fields?.find(field => field.key === key)?.value ?? fallback
    setTemplate(value('compose.template', 'clean')); setSignatureID(value('compose.signature_id', '')); setUseSignature(value('compose.use_signature', 'true') === 'true')
    setTone(value('compose.tone', 'professional')); setLength(value('compose.reply_length', 'short')); setLanguage(value('compose.language', 'ko'))
    if (!id && !dirtyRef.current && !current) { const format = value('compose.format', 'auto'); setMode(format === 'text' ? 'plain' : format === 'html' ? 'html' : format === 'markdown' ? 'markdown' : 'auto') }
  }, [preferences.data, fields.account, id, current])

  function applyView(view: DraftView) {
    loaded.current = `${view.draft.id}:${view.version.version}`
    setCurrent(view)
    setFields({ account: view.draft.account_id, to: addresses(view.version.to), cc: addresses(view.version.cc), bcc: addresses(view.version.bcc), subject: view.version.subject, body: view.version.body_text, html: view.version.body_html || textToHTML(view.version.body_text) })
    setDirty(false)
    cache.setQueryData(['draft', view.draft.id], view)
    cache.invalidateQueries({ queryKey: ['drafts'] })
  }
  useEffect(() => {
    if (previousRoute.current !== id) {
      previousRoute.current = id
      if (!id || current?.draft.id !== id) { loaded.current = ''; setFields(empty); setCurrent(undefined); setDirty(false); setMode('html'); setPreview(undefined); setApproval(undefined); setOutbound(undefined); setSendUncertain(false) }
    }
  }, [id])
  useEffect(() => {
    if (draft.data && loaded.current !== `${draft.data.draft.id}:${draft.data.version.version}` && !dirty) {
      applyView(draft.data)
      setMode(draft.data.version.body_html ? 'html' : 'plain')
    }
    // A background refetch must never overwrite unsaved local edits.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [draft.data, dirty])
  useEffect(() => {
    if (!id && !current && !fields.account && accounts.data?.length && !personalPreferences.isPending) setFields(value => ({ ...value, account: defaultComposeAccount(accounts.data, preferredAccount, explicitAccount) }))
  }, [accounts.data, current, fields.account, id, personalPreferences.isPending, preferredAccount, explicitAccount])
  useEffect(() => {
    if (!dirty) return
    const warn = (event: BeforeUnloadEvent) => { if (dirtyRef.current) { event.preventDefault(); event.returnValue = '' } }
    window.addEventListener('beforeunload', warn)
    return () => window.removeEventListener('beforeunload', warn)
  }, [dirty])

  function edit(update: Partial<Fields>) { setFields(value => ({ ...value, ...update })); setDirty(true); setPreview(undefined); setApproval(undefined) }
  function switchMode(next: typeof mode) {
    const nextText = next === 'plain' || next === 'markdown' || next === 'auto'
    const currentText = mode === 'plain' || mode === 'markdown' || mode === 'auto'
    if (nextText && !currentText) {
      // Read only text from an inert DOM. It is never mounted into the page.
      const document = new DOMParser().parseFromString(fields.html, 'text/html')
      document.querySelectorAll('script,style,iframe,object,svg').forEach(element => element.remove())
      document.querySelectorAll('p,div,h1,h2,h3,li,blockquote,br,tr').forEach(element => element.append('\n'))
      edit({ body: document.body.textContent?.trim() || '', html: '' })
    } else if (!nextText && currentText) edit({ html: textToHTML(fields.body) })
    else { setDirty(true); setPreview(undefined); setApproval(undefined) }
    setMode(next)
  }
  function renderOptions() { return { format: mode === 'plain' ? 'text' : mode === 'source' || mode === 'html' ? 'html' : mode, template: mode === 'plain' ? 'plain' : template, signature_id: signatureID, use_signature: useSignature, tone, length, language } }
  function bodyOptions() { return { body: fields.body, body_html: mode === 'html' || mode === 'source' ? fields.html : '' } }
  function changeFormatOption(change: () => void) { change(); setDirty(true); setPreview(undefined); setApproval(undefined) }
  async function saveDraft(): Promise<DraftView> {
    if (!fields.account) throw new Error('발신 계정을 선택해 주세요.')
    if (!dirty && current) return current
    const body = { to: splitRecipients(fields.to), cc: splitRecipients(fields.cc), bcc: splitRecipients(fields.bcc), subject: fields.subject, ...bodyOptions(), ...renderOptions() }
    let value: DraftView
    if (current) value = await api<DraftView>(`/api/drafts/${current.draft.id}`, { method: 'PATCH', body: body satisfies UpdateDraftInput })
    else {
      value = await api<DraftView>('/api/drafts', { method: 'POST', body: { ...body, account_id: fields.account, kind: 'new' } satisfies CreateDraftInput })
    }
    applyView(value)
    setMode(value.version.body_html ? 'html' : 'plain')
    if (!id) navigate(`/drafts/${value.draft.id}`, { replace: true })
    return value
  }
  const operation = useMutation({
    mutationFn: async (intent: 'save' | 'preview' | 'short' | 'polite' | 'business' | 'rewrite') => {
      setApproval(undefined)
      const view = await saveDraft()
      if (intent === 'preview') { const value = await api<SendPreview>(`/api/drafts/${view.draft.id}/preview`, { method: 'POST', body: {} }); setPreview(value) }
      else if (intent !== 'save') {
        const styles = { short: '짧고 간결하게', polite: '정중하고 공손하게', business: '명확한 업무 문체로', rewrite: '핵심 내용을 유지하며 다시 작성' }
        const value = await api<DraftView>(`/api/drafts/${view.draft.id}`, { method: 'PATCH', body: { ...renderOptions(), format: 'auto', smart_format: true, intent: styles[intent] } })
        applyView(value); setMode(value.version.body_html ? 'html' : 'plain'); toast.success('AI가 새 버전으로 작성했습니다. 내용을 검토해 주세요.')
      } else toast.success('초안을 저장했습니다.')
    },
  })
  const formatMail = useMutation({ mutationFn: async () => {
    const reply = current?.draft
    const value = await api<RenderedMail>('/api/mail/render', { method: 'POST', body: { account_id: fields.account, ...bodyOptions(), ...renderOptions(), reply_to_message_id: reply?.kind === 'reply' || reply?.kind === 'reply_all' ? reply.reply_to_message_id : undefined } satisfies RenderMailInput })
    edit({ body: value.body_text, html: value.body_html || '' }); setMode(value.body_html ? 'html' : 'plain')
    value.warnings?.forEach(warning => toast.message(warning))
    toast.success('서식을 적용했습니다. 저장하거나 미리보기에서 확인해 주세요.')
  } })
  const selectionRewrite = useMutation({ mutationFn: async (text: string) => {
    const value = await api<{ text: string }>('/api/mail/rewrite-selection', { method: 'POST', body: { account_id: fields.account, text, tone, length, language, instruction: '선택한 문장만 원래 의미를 유지해 자연스럽게 다듬기' } })
    return value.text
  } })
  const attachment = useMutation({ mutationFn: async (input: { files?: File[]; inline?: boolean; remove?: string }) => {
    setApproval(undefined); setPreview(undefined)
    let view = await saveDraft()
    if (input.remove) view = await api<DraftView>(`/api/drafts/${encodeURIComponent(view.draft.id)}/attachments/${encodeURIComponent(input.remove)}`, { method: 'DELETE' })
    else for (const file of input.files || []) {
      if (file.size > 64 * 1024 * 1024) throw new Error('첨부파일은 최대 64 MiB까지 업로드할 수 있습니다. 조직 정책은 더 작을 수 있습니다.')
      view = await api<DraftView>(`/api/drafts/${encodeURIComponent(view.draft.id)}/attachments`, { method: 'POST', body: { name: file.name, data_base64: await fileBase64(file), inline: !!input.inline } })
      applyView(view)
    }
    applyView(view); setMode(view.version.body_html ? 'html' : 'plain')
    toast.success(input.remove ? '현재 초안에서 첨부파일을 제거했습니다. 이전 버전은 유지됩니다.' : '검사를 통과한 첨부파일을 저장했습니다.')
  } })
  const approve = useMutation({
    mutationFn: async () => {
      if (!preview || dirty) throw new Error('변경된 내용을 먼저 미리보기에서 확인해 주세요.')
      const result = await api<{ preview: SendPreview; approval: Approval }>(`/api/drafts/${preview.draft_id}/request-approval`, { method: 'POST', body: { ttl_seconds: 600 } })
      if (result.preview.payload_hash !== preview.payload_hash) {
        setPreview(result.preview); setApproval(undefined)
        throw new Error('다른 화면에서 초안이 변경되었습니다. 새 미리보기를 확인한 뒤 다시 승인해 주세요.')
      }
      setApproval(result.approval)
    },
  })
  const send = useMutation({
    retry: false,
    mutationFn: async () => {
      sendDispatched.current = false
      if (!preview || !approval || dirty || preview.dlp_blocked) throw new Error('검토한 미리보기의 발송 승인이 필요합니다.')
      if (approval.expires * 1000 < Date.now()) { setApproval(undefined); throw new Error('승인이 만료되었습니다. 다시 승인해 주세요.') }
      sendDispatched.current = true
      return api<Outbound>(`/api/drafts/${preview.draft_id}/send`, { method: 'POST', body: { approval_token: approval.token, idempotency_key: `draft:${preview.draft_id}:v${preview.draft_version}` } })
    },
    onSuccess: result => { setOutbound(result); setPreview(undefined); setApproval(undefined); setDirty(false); cache.invalidateQueries({ queryKey: ['drafts'] }); cache.invalidateQueries({ queryKey: ['outbound'] }); cache.invalidateQueries({ queryKey: ['draft', id] }) },
    onError: () => { if (sendDispatched.current) setSendUncertain(true); setApproval(undefined) },
  })
  const busy = operation.isPending || approve.isPending || send.isPending || formatMail.isPending || selectionRewrite.isPending || attachment.isPending
  useEffect(() => {
    const beforeLogout = (event: Event) => {
      if (busy) {
        event.preventDefault()
        toast.error('초안 저장 또는 발송 요청을 처리하고 있습니다. 완료 후 로그아웃해 주세요.')
        return
      }
      if (!dirtyRef.current) return
      if (!window.confirm('저장하지 않은 초안 변경이 사라집니다. 로그아웃하시겠습니까?')) {
        event.preventDefault()
        return
      }
      logoutWasDirty.current = true
      setDirty(false)
    }
    const logoutFailed = () => {
      if (logoutWasDirty.current) { logoutWasDirty.current = false; setDirty(true) }
    }
    window.addEventListener('postra:before-logout', beforeLogout)
    window.addEventListener('postra:logout-failed', logoutFailed)
    return () => {
      window.removeEventListener('postra:before-logout', beforeLogout)
      window.removeEventListener('postra:logout-failed', logoutFailed)
    }
  }, [busy])
  const closed = current && current.draft.status !== 'open'
  if (accounts.isPending || (id && draft.isPending)) return <Loading />
  if (accounts.error) return <ErrorState error={accounts.error} retry={() => accounts.refetch()} />
  if (id && draft.error) return <ErrorState error={draft.error} retry={() => draft.refetch()} />
  if (!accounts.data?.length) return <EmptyState title="발신 계정이 필요합니다" description="계정 프로비저닝 또는 동기화 설정을 확인해 주세요." action={<Button asChild><Link to="/accounts">계정 확인</Link></Button>} />
  if (outbound) return <div className="page compose-result"><CheckCircle2 size={40} /><h1>{outbound.status === 'sent' ? '메일을 발송했습니다' : outbound.status === 'retry_wait' || outbound.status === 'queued' ? '발송 처리 중입니다' : outbound.status === 'send_uncertain' ? '발송 결과 확인이 필요합니다' : '메일 발송에 실패했습니다'}</h1><p className="muted">상태: {outbound.status}. 발송 기록에서 전달 상태를 확인할 수 있습니다.</p><p className="muted">중복 발송을 막기 위해 이 화면에서 자동 재발송하지 않습니다.</p><div className="row"><Button asChild><Link to="/sent">발송 기록</Link></Button><Button variant="outline" asChild><Link to="/mail">받은메일</Link></Button></div></div>
  return <div className="page compose-page">
    <PageHeader title={current ? '메일 초안' : '새 메일 작성'} description="마음이 전해지는 메일, 정확하게 승인하고 발송하세요." actions={<Badge variant="outline">{dirty ? '저장하지 않은 변경' : current ? `버전 ${current.version.version} · 저장됨` : '새 초안'}</Badge>} />
    {operation.error && <ErrorState error={operation.error} />}
    {formatMail.error && <ErrorState error={formatMail.error} />}
    {attachment.error && <ErrorState error={attachment.error} />}
    {preferences.error && <ErrorState error={preferences.error} retry={() => preferences.refetch()} />}
    {!id && personalPreferences.error && <ErrorState error={personalPreferences.error} retry={() => personalPreferences.refetch()} />}
    {sendUncertain && <p className="preview-warning" role="alert">발송 요청의 결과 확인이 필요하여 편집을 잠시 잠갔습니다. 중복 발송을 막기 위해 <Link to="/sent">발송 기록</Link>을 먼저 확인해 주세요.</p>}
    {current && (!id || current.draft.id === id) && <DraftSourcePanel key={`${current.draft.id}:${typeof current.draft.reply_to_message_id === 'string' ? current.draft.reply_to_message_id : ''}`} draftID={current.draft.id} kind={current.draft.kind} messageID={current.draft.reply_to_message_id}/>}
    {closed ? <section className="compose-closed"><Badge>{current.draft.status}</Badge><h2>{current.version.subject}</h2><p>받는 사람: {addresses(current.version.to)}</p><DraftBody draftID={current.draft.id} version={current.version.version} html={current.version.body_html} text={current.version.body_text} attachments={current.version.attachments}/>{current.version.attachments?.map(file => <p key={file.id}><a href={attachmentURL(current.draft.id, file.id, current.version.version)} download>{file.name}</a></p>)}<Button asChild variant="outline"><Link to="/sent">발송 기록 확인</Link></Button></section> : <>
      <fieldset className="compose-fields" disabled={busy || sendUncertain}>
        <label className="compose-field"><span>보내는 사람</span><select aria-label="발신 계정" value={fields.account} disabled={!!current} onChange={event => edit({ account: event.target.value })}><option value="" disabled>발신 계정 선택</option>{accounts.data.map(account => <option key={account.id} value={account.id}>{account.name || account.email} &lt;{account.email}&gt;{account.status !== 'active' ? ` · ${account.status}` : ''}</option>)}</select></label>
        <label className="compose-field"><span>받는 사람</span><Input aria-label="받는 사람" value={fields.to} placeholder="hong@corp.local, kim@corp.local" autoComplete="off" onChange={event => edit({ to: event.target.value })} /></label>
        <div className="compose-copy-fields"><label className="compose-field"><span>참조</span><Input aria-label="참조" value={fields.cc} placeholder="선택사항" onChange={event => edit({ cc: event.target.value })} /></label><label className="compose-field"><span>숨은참조</span><Input aria-label="숨은참조" value={fields.bcc} placeholder="선택사항" onChange={event => edit({ bcc: event.target.value })} /></label></div>
        <label className="compose-field"><span>제목</span><Input aria-label="메일 제목" value={fields.subject} placeholder="메일의 제목을 입력하세요" onChange={event => edit({ subject: event.target.value })} /></label>
        <div className="compose-mode-bar"><div className="row"><FileText size={16} /><span>메일 본문</span></div><select aria-label="본문 작성 방식" value={mode} disabled={locked('compose.format')} onChange={event => switchMode(event.target.value as typeof mode)}><option value="html">서식 편집</option><option value="source">HTML 직접 입력</option><option value="auto">자동 감지 · 텍스트 입력</option><option value="markdown">Markdown</option><option value="plain">일반 텍스트</option></select></div>
        {mode === 'html' ? <div onPasteCapture={event => { const images = Array.from(event.clipboardData.files).filter(file => /^image\/(png|jpeg|gif)$/.test(file.type)); if (images.length) { event.preventDefault(); if (!busy && !sendUncertain) attachment.mutate({ files: images, inline: true }) } }}><RichEditor value={fields.html} disabled={busy || sendUncertain} onChange={(html, body) => edit({ html, body })} onRewriteSelection={text => selectionRewrite.mutateAsync(text)} /></div> : <Textarea className={mode === 'source' ? 'compose-source' : 'compose-plain'} aria-label={mode === 'source' ? 'HTML 본문' : mode === 'markdown' ? 'Markdown 본문' : mode === 'auto' ? '자동 서식 본문' : '일반 텍스트 본문'} value={mode === 'source' ? fields.html : fields.body} onChange={event => edit(mode === 'source' ? { html: event.target.value } : { body: event.target.value })} />}
        <div className="compose-templates"><label>템플릿 <select aria-label="메일 템플릿" value={template} disabled={locked('compose.template') || mode === 'plain'} onChange={event => changeFormatOption(() => setTemplate(event.target.value))}>{(templates.data || [{ id: 'clean', name: '기본', description: '' }]).map(item => <option key={item.id} value={item.id}>{item.name}</option>)}</select></label><label><input type="checkbox" checked={useSignature} disabled={locked('compose.use_signature')} onChange={event => changeFormatOption(() => setUseSignature(event.target.checked))} /> 서명 적용</label><select aria-label="메일 서명" value={signatureID} disabled={!useSignature || locked('compose.signature_id')} onChange={event => changeFormatOption(() => setSignatureID(event.target.value))}><option value="">서명 선택</option>{signatures.data?.filter(signature => !signature.account_id || signature.account_id === fields.account).map(signature => <option key={signature.id} value={signature.id}>{signature.name}</option>)}</select><Link to="/settings/signatures">서명 관리</Link><Button size="sm" variant="outline" onClick={() => formatMail.mutate()}>서식 적용</Button><span className="muted">자동 서식은 AI 연결 없이 동작합니다.</span></div>
      </fieldset>
      <fieldset className="compose-attachments" disabled={busy || sendUncertain}><legend>첨부파일 · 본문 이미지</legend><div className="row"><label>파일 첨부 <input type="file" multiple aria-label="첨부파일 선택" onChange={event => { const files = Array.from(event.target.files || []); event.target.value = ''; if (files.length) attachment.mutate({ files }) }} /></label><label>본문 이미지 <input type="file" accept="image/png,image/jpeg,image/gif" aria-label="본문 이미지 선택" onChange={event => { const files = Array.from(event.target.files || []); event.target.value = ''; if (files.length) attachment.mutate({ files, inline: true }) }} /></label></div><p className="muted">이미지는 서식 편집기에 붙여넣을 수도 있습니다. 업로드 전에 초안을 저장하고 파일을 검사하며, 변경 후에는 다시 승인해야 합니다.</p>{current?.version.attachments?.map(file => <div className="compose-attachment-item" key={file.id}><a href={attachmentURL(current.draft.id, file.id, current.version.version)} download>{file.name}</a><span>{Math.ceil(file.size / 1024)} KB · {file.inline ? '본문 이미지' : '첨부'} · {file.scan_status}</span><Button size="icon" variant="ghost" aria-label={`${file.name} 첨부 제거`} onClick={() => attachment.mutate({ remove: file.id })}><Trash2 size={15} /></Button></div>)}</fieldset>
      <div className="compose-ai-bar"><span><Sparkles size={16} /> AI 다듬기</span>{([['short', '짧게'], ['polite', '공손하게'], ['business', '업무식'], ['rewrite', '다시 작성']] as const).map(([style, label]) => <Button key={style} size="sm" variant="ghost" disabled={busy || sendUncertain || (!fields.body.trim() && !fields.html.trim())} onClick={() => operation.mutate(style)}>{label}</Button>)}<p className="muted">새 버전으로 저장하며, 결과를 확인하기 전에는 발송하지 않습니다.</p></div>
      <fieldset className="compose-ai-options" disabled={busy || sendUncertain}><label>톤 <select aria-label="AI 작성 톤" value={tone} disabled={locked('compose.tone')} onChange={event => setTone(event.target.value)}><option value="professional">업무용</option><option value="formal">격식</option><option value="friendly">친근하게</option><option value="concise">간결하게</option><option value="apologetic">정중한 사과</option><option value="persuasive">설득력 있게</option><option value="executive">경영진 보고</option></select></label><label>길이 <select aria-label="AI 작성 길이" value={length} disabled={locked('compose.reply_length')} onChange={event => setLength(event.target.value)}><option value="short">짧게</option><option value="medium">보통</option><option value="long">상세히</option></select></label><label>언어 <select aria-label="AI 작성 언어" value={language} disabled={locked('compose.language')} onChange={event => setLanguage(event.target.value)}><option value="ko">한국어</option><option value="en">영어</option></select></label></fieldset>
      <p className="compose-help muted">서식 메일은 HTML과 일반 텍스트를 함께 발송합니다. 위험한 코드는 제거하며 외부 이미지는 관리자 정책을 따릅니다. 로컬 미리보기에서는 외부 이미지를 차단하고, 검사한 본문 이미지만 CID 첨부로 표시합니다. 바이너리 첨부의 민감정보는 직접 검토해 주세요.</p>
      <footer className="compose-footer"><Button variant="outline" disabled={busy || sendUncertain} onClick={() => operation.mutate('save')}><Save size={16} /> 초안 저장</Button><span className="muted">{current?.version.author === 'ai' ? 'AI 작성 초안 · 직접 검토 필요' : '발송에는 별도 승인이 필요합니다'}</span><Button disabled={busy || sendUncertain} onClick={() => operation.mutate('preview')}><Eye size={17} />{operation.isPending ? '처리 중…' : '미리보기 · 발송'}</Button></footer>
    </>}
    <Dialog.Root open={!!preview} onOpenChange={open => { if (!open && !busy) { setPreview(undefined); setApproval(undefined) } }}><Dialog.Portal><Dialog.Overlay className="compose-dialog-overlay" /><Dialog.Content className="compose-preview-dialog" onEscapeKeyDown={event => { if (busy) event.preventDefault() }} onPointerDownOutside={event => event.preventDefault()}>
      <header className="compose-preview-header"><div><Dialog.Title>발송 전 최종 확인</Dialog.Title><Dialog.Description>수신자와 본문, 보안 검사를 확인하고 승인해 주세요.</Dialog.Description></div><Button variant="ghost" size="icon" disabled={busy} aria-label="미리보기 닫기" onClick={() => { setPreview(undefined); setApproval(undefined) }}><X size={19} /></Button></header>
      {preview && <div className="compose-preview-scroll"><div className="preview-security"><div><strong>{preview.recipient_count}</strong><span>전체 수신자</span></div><div><strong>{preview.external_domains?.length ?? 0}</strong><span>외부 도메인</span></div><div><strong>{preview.dlp_findings?.length ?? 0}</strong><span>민감정보 유형</span></div><div><strong>v{preview.draft_version}</strong><span>승인 대상 버전</span></div></div><div className="preview-envelope"><h2>{preview.subject}</h2><p>보내는 사람: {preview.from}</p><p>받는 사람: {preview.to.join(', ')}</p>{!!preview.cc?.length && <p>참조: {preview.cc.join(', ')}</p>}{!!preview.bcc?.length && <p>숨은참조: {preview.bcc.join(', ')}</p>}{preview.external_domains?.length ? <p className="warning-text">외부 도메인: {preview.external_domains.join(', ')}</p> : null}</div>
        {preview.warnings?.map((warning, i) => <p className="preview-warning" key={i}>{warning}</p>)}{preview.dlp_blocked && <ErrorState error={new Error('조직의 DLP 정책에 따라 발송이 차단되었습니다. 민감정보를 수정한 뒤 다시 확인해 주세요.')} />}
        {!!preview.attachments?.length && <div className="preview-envelope"><h3>첨부파일 {preview.attachments.length}개</h3>{preview.attachments.map(file => <p key={file.id}><a href={attachmentURL(preview.draft_id, file.id, preview.draft_version)} download>{file.name}</a> · {Math.ceil(file.size / 1024)} KB · {file.inline ? '본문 이미지' : '첨부'}</p>)}</div>}
        <DraftBody html={preview.body_html} text={preview.body} draftID={preview.draft_id} version={preview.draft_version} attachments={preview.attachments} title="서식 메일 미리보기" />{preview.body_html && <details className="preview-plain"><summary>일반 텍스트 대체 본문</summary><pre>{preview.body}</pre></details>}
        {approve.error && <ErrorState error={approve.error} />}{send.error && <ErrorState error={send.error} />}{sendUncertain && <p role="alert" className="preview-warning">발송 요청의 처리 여부를 확정할 수 없습니다. 중복 전송을 방지하기 위해 <Link to="/sent">발송 기록</Link>을 먼저 확인하세요.</p>}
      </div>}
      <footer className="compose-preview-footer">{approval ? <><span className="approved-hint"><ShieldCheck size={17} /> 검토 승인됨 · 10분 내 발송</span><Button disabled={busy || sendUncertain || preview?.dlp_blocked} onClick={() => send.mutate()}><Send size={17} />{send.isPending ? '발송 중…' : '승인한 메일 발송'}</Button></> : <><Button variant="outline" disabled={busy} onClick={() => { setPreview(undefined); setApproval(undefined) }}>돌아가서 수정</Button><Button disabled={busy || sendUncertain || preview?.dlp_blocked} onClick={() => approve.mutate()}><ShieldCheck size={17} />{approve.isPending ? '승인 중…' : '내용 확인 · 승인'}</Button></>}</footer>
    </Dialog.Content></Dialog.Portal></Dialog.Root>
    <Dialog.Root open={blocker.state === 'blocked'} onOpenChange={open => { if (!open && blocker.state === 'blocked') blocker.reset() }}><Dialog.Portal><Dialog.Overlay className="compose-dialog-overlay" /><Dialog.Content className="compose-preview-dialog draft-leave-dialog"><Dialog.Title>저장하지 않은 초안이 있습니다</Dialog.Title><Dialog.Description>이 화면을 나가면 저장하지 않은 제목, 수신자, 본문 변경이 사라집니다.</Dialog.Description><div className="row"><Button variant="outline" onClick={() => { if (blocker.state === 'blocked') blocker.reset() }}>계속 작성</Button><Button variant="destructive" disabled={busy} onClick={() => { if (blocker.state === 'blocked') { setDirty(false); blocker.proceed() } }}>변경을 버리고 이동</Button></div></Dialog.Content></Dialog.Portal></Dialog.Root>
  </div>
}
