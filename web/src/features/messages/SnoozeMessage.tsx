import {useRef, useState, type FormEvent} from 'react'
import * as Dialog from '@radix-ui/react-dialog'
import {useMutation, useQueryClient} from '@tanstack/react-query'
import {Clock, X} from 'lucide-react'
import {toast} from 'sonner'
import {api} from '@/api/client'
import {Button, ErrorState, Input} from '@/components/ui'
import {batchResponse} from './responses'
import {snoozeChoices, snoozeDateLabel, snoozeTimestamp, type SnoozePreset} from './snooze'
import './snooze.css'

export function SnoozeMessage({messageID, snoozedUntil = 0, disabled = false}: {messageID: string; snoozedUntil?: number; disabled?: boolean}) {
  const [open, setOpen] = useState(false)
  const [preset, setPreset] = useState<SnoozePreset>('1')
  const [custom, setCustom] = useState('')
  const [error, setError] = useState<Error | null>(null)
  const inFlight = useRef(false)
  const cache = useQueryClient()
  const mutation = useMutation({
    mutationFn: async (until: number) => {
      const result = batchResponse(await api('/api/messages/batch', {method: 'POST', body: {message_ids: [messageID], action: until ? 'snooze' : 'unsnooze', ...(until ? {snoozed_until: until} : {})}}), [messageID])
      if (result.failed) throw new Error(result.results[0].error || '다시 보기 예약을 변경하지 못했습니다.')
      return result
    },
    onSuccess: (_, until) => {
      for (const queryKey of [['message', messageID], ['messages'], ['work'], ['thread']]) void cache.invalidateQueries({queryKey})
      setOpen(false)
      toast.success(until ? `${snoozeDateLabel(until)}에 다시 표시하도록 예약했습니다. 받은메일 목록은 최대 1분 간격으로 갱신됩니다.` : '다시 보기 예약을 해제했습니다.')
    },
    onSettled: () => {inFlight.current = false},
  })
  function changeOpen(next: boolean) {
    if (inFlight.current) return
    if (next) {setError(null); mutation.reset(); setPreset('1'); setCustom('')}
    setOpen(next)
  }
  function save(until: number) {
    if (disabled || inFlight.current) return
    setError(null); inFlight.current = true; mutation.mutate(until)
  }
  function submit(event: FormEvent) {
    event.preventDefault()
    try {save(snoozeTimestamp(preset, custom))}
    catch (reason) {setError(reason instanceof Error ? reason : new Error('예약 시각을 확인해 주세요.'))}
  }
  const current = snoozedUntil > 0 ? snoozeDateLabel(snoozedUntil) : ''
  return <div className="snooze-message"><Dialog.Root open={open} onOpenChange={changeOpen}>
    <Dialog.Trigger asChild><Button size="sm" variant="ghost" disabled={disabled}><Clock size={16}/>{current ? '다시 보기 변경' : '나중에 다시 보기'}</Button></Dialog.Trigger>
    {current && <span className="snooze-current small" role="status">{snoozedUntil * 1000 <= Date.now() ? '다시 확인할 시각이 지났습니다: ' : '다시 보기 예약: '}<time dateTime={new Date(snoozedUntil * 1000).toISOString()}>{current}</time></span>}
    <Dialog.Portal><Dialog.Overlay className="dialog-overlay"/><Dialog.Content className="snooze-dialog" onEscapeKeyDown={event => {if (inFlight.current) event.preventDefault()}} onPointerDownOutside={event => {if (inFlight.current) event.preventDefault()}}>
      <div className="row between"><Dialog.Title>메일을 나중에 다시 보기</Dialog.Title><Dialog.Close asChild><Button size="icon" variant="ghost" aria-label="다시 보기 닫기" disabled={mutation.isPending}><X size={17}/></Button></Dialog.Close></div>
      <Dialog.Description className="snooze-description muted small">예약 시각까지 받은메일에서 잠시 숨깁니다. 열려 있는 받은메일·다시 알림 목록은 최대 1분 간격으로 갱신되며, 백그라운드에서는 자동 갱신을 쉽니다. 현지 시간대 기준이며, 푸시 알림이나 메일 발송은 하지 않습니다. 보관된 메일은 보관을 해제해야 받은메일에 나타납니다.</Dialog.Description>
      {current && <p className="small">현재 예약: {current}</p>}
      <form className="stack" onSubmit={submit} noValidate>
        <label className="field">다시 볼 시각<select className="input" value={preset} disabled={mutation.isPending} onChange={event => {setPreset(event.target.value as SnoozePreset); setError(null)}}>{snoozeChoices.map(choice => <option key={choice.value} value={choice.value}>{choice.label}</option>)}</select></label>
        {preset === 'custom' && <label className="field">직접 지정 날짜·시각<Input type="datetime-local" value={custom} disabled={mutation.isPending} onChange={event => {setCustom(event.target.value); setError(null)}}/></label>}
        {(error || mutation.error) && <ErrorState error={error || mutation.error}/>}
        <div className="row"><Button type="submit" disabled={mutation.isPending || disabled}>{mutation.isPending ? '저장 중…' : '다시 보기 예약'}</Button>{current && <Button variant="outline" disabled={mutation.isPending || disabled} onClick={() => save(0)}>예약 해제</Button>}</div>
      </form>
    </Dialog.Content></Dialog.Portal>
  </Dialog.Root></div>
}
