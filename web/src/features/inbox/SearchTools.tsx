import {useState, type FormEvent} from 'react'
import {useMutation, useQuery, useQueryClient} from '@tanstack/react-query'
import {api} from '@/api/client'
import {Button, ErrorState, Input} from '@/components/ui'
import {accountListResponse, batchResponse} from '../messages/responses'
import {snoozeChoices, snoozeTimestamp, type SnoozePreset} from '../messages/snooze'

export function keywordSearchParams(params: URLSearchParams, cursor: string): URLSearchParams {
  const folder = params.get('folder') || 'inbox'
  const query = new URLSearchParams({q: params.get('q') || '', limit: '60', cursor, folder: ['important', 'archive', 'snoozed'].includes(folder) ? folder : 'inbox'})
  const account = params.get('account'); if (account) query.set('account_id', account)
  if (folder === 'unread') query.set('is_read','false')
  for (const name of ['from', 'to', 'subject', 'label']) {const value = params.get(name); if (value) query.set(name, value)}
  for (const name of ['since', 'until']) {
    const date = params.get(name); if (!date) continue
    const value = new Date(date + (name === 'until' ? 'T23:59:59' : 'T00:00:00')).getTime()
    if (Number.isFinite(value)) query.set(name, String(Math.floor(value / 1000)))
  }
  if (folder === 'attachment' || params.get('has_attachment') === 'true') query.set('has_attachment', 'true')
  if (folder === 'today') {const today = new Date(); today.setHours(0, 0, 0, 0); query.set('since', String(Math.floor(today.getTime() / 1000)))}
  return query
}

export function AdvancedSearch({params, onChange}: {params: URLSearchParams; onChange: (values: Record<string,string>) => void}) {
  const accounts = useQuery({queryKey: ['accounts'], queryFn: ({signal}) => api<unknown>('/api/accounts', {signal}).then(accountListResponse)})
  function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault(); const form = new FormData(event.currentTarget)
    onChange(Object.fromEntries(['account', 'from', 'to', 'subject', 'label', 'since', 'until', 'has_attachment'].map(name => [name, String(form.get(name) || '')])))
  }
  return <details className="advanced-mail-search"><summary>상세 검색 조건</summary><form className="stack" onSubmit={submit} key={['account','from','to','subject','label','since','until','has_attachment'].map(name => params.get(name)).join('|')}>
    <label className="field">메일 계정<select className="input" name="account" defaultValue={params.get('account') || ''}><option value="">모든 내 계정</option>{accounts.data?.map(account => <option key={account.id} value={account.id}>{account.name} · {account.email}</option>)}</select></label>
    {accounts.error && <ErrorState error={accounts.error}/>}
    {[['from','보낸이'],['to','받는이'],['subject','제목'],['label','라벨']].map(([name,label]) => <label className="field" key={name}>{label}<Input name={name} defaultValue={params.get(name) || ''}/></label>)}
    <div className="grid"><label className="field">시작일<Input name="since" type="date" defaultValue={params.get('since') || ''}/></label><label className="field">종료일<Input name="until" type="date" defaultValue={params.get('until') || ''}/></label></div>
    <label className="row"><input type="checkbox" name="has_attachment" value="true" defaultChecked={params.get('has_attachment') === 'true'}/>첨부파일 있음</label>
    <div className="row"><Button size="sm" type="submit">조건 적용</Button><Button size="sm" variant="ghost" onClick={() => onChange({account:'',from:'',to:'',subject:'',label:'',since:'',until:'',has_attachment:''})}>조건 초기화</Button></div>
  </form></details>
}

export function BulkActions({ids, onSuccess}: {ids: string[]; onSuccess: (successfulIDs: string[]) => void}) {
  const cache = useQueryClient()
  const [action, setAction] = useState('archive')
  const [days, setDays] = useState<SnoozePreset>('1')
  const [custom, setCustom] = useState('')
  const [label, setLabel] = useState('')
  const batch = useMutation({mutationFn: () => {
    const snoozedUntil = action === 'snooze' ? snoozeTimestamp(days, custom) : 0
    return api<unknown>('/api/messages/batch', {body: {message_ids: ids, action, ...(action === 'snooze' ? {snoozed_until: snoozedUntil} : {}), ...(['add_label','remove_label'].includes(action) ? {label} : {})}}).then(value => batchResponse(value, ids))
  }, onSuccess: result => {
    onSuccess(result.results.filter(item => item.ok).map(item => item.message_id))
    void cache.invalidateQueries({queryKey: ['messages']}); void cache.invalidateQueries({queryKey: ['message']}); void cache.invalidateQueries({queryKey: ['work']}); void cache.invalidateQueries({queryKey: ['thread']})
  }})
  if (!ids.length && !batch.data && !batch.error) return null
  return <div className="mail-bulk-actions"><div className="row"><span className="small">{ids.length}개 선택</span><select className="input" aria-label="선택 메일 작업" value={action} disabled={batch.isPending} onChange={event => setAction(event.target.value)}>{[['archive','보관'],['unarchive','보관 해제'],['mark_read','읽음 표시'],['mark_unread','안읽음 표시'],['mark_important','중요 표시'],['unmark_important','중요 해제'],['snooze','다시 알림'],['unsnooze','다시 알림 해제'],['add_label','라벨 추가'],['remove_label','라벨 제거'],['delete','로컬 삭제']].map(([value,name]) => <option key={value} value={value}>{name}</option>)}</select></div>
    {action === 'snooze' && <><select className="input" aria-label="다시 알림 시각" value={days} disabled={batch.isPending} onChange={event => {setDays(event.target.value as SnoozePreset); batch.reset()}}>{snoozeChoices.map(choice => <option key={choice.value} value={choice.value}>{choice.label}</option>)}</select>{days === 'custom' && <label className="field">직접 지정 날짜·시각<Input type="datetime-local" value={custom} disabled={batch.isPending} onChange={event => {setCustom(event.target.value); batch.reset()}}/></label>}<p className="small muted">현지 시간대 기준입니다. 열려 있는 받은메일·다시 알림 목록은 최대 1분 간격으로 갱신됩니다. 푸시 알림·메일 발송은 하지 않습니다.</p></>}
    {['add_label','remove_label'].includes(action) && <Input aria-label="변경할 라벨" value={label} onChange={event => setLabel(event.target.value)}/>}
    <Button size="sm" variant={action === 'delete' ? 'destructive' : 'outline'} disabled={!ids.length || batch.isPending || ['add_label','remove_label'].includes(action) && !label.trim()} onClick={() => {if (action === 'delete' && !window.confirm(`선택한 ${ids.length}개 메일의 Postra 데이터를 삭제하시겠습니까? 메일 서버의 원본은 유지됩니다.`)) return; batch.mutate()}}>{batch.isPending ? '처리 중…' : '선택 메일에 적용'}</Button>
    {batch.error && <ErrorState error={batch.error}/>}{batch.data && <p className="small" role="status">{batch.data.succeeded}개 처리 · {batch.data.failed}개 실패{batch.data.failed > 0 ? ' · 실패한 메일을 선택된 상태로 유지했습니다.' : ''}</p>}{!!batch.data?.failed && <ErrorState error={new Error(batch.data.results.filter(item => !item.ok).map(item => item.error || '처리 실패').join(' · '))}/>}
  </div>
}
