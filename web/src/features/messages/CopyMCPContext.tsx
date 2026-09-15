import { useState } from 'react'
import * as Dialog from '@radix-ui/react-dialog'
import { Copy, X } from 'lucide-react'
import { toast } from 'sonner'
import { Button } from '@/components/ui'
import type { Message } from './types'

export function messageMCPContext(message: Message): string {
  // No body, subject, recipients, sender, attachment data, credentials, or send
  // approval token is exported. The MCP server rechecks the caller's ownership.
  return JSON.stringify({
    product: 'Postra', kind: 'message_reference',
    account_id: message.account_id, message_id: message.id,
    ...(message.thread_id ? { thread_id: message.thread_id } : {}),
    read: { tool: 'mail_message_get', arguments: { message_id: message.id } },
    reply_draft: { tool: 'mail_draft_create', arguments: { account_id: message.account_id, kind: 'reply', reply_to_message_id: message.id, format: 'auto' } },
    safety: 'References only. Authenticate with your own Postra MCP key. Do not send mail without reviewing the draft and obtaining explicit approval.',
  }, null, 2)
}

export function CopyMCPContext({ message, open, onOpenChange }: { message: Message; open: boolean; onOpenChange: (value: boolean) => void }) {
  const context = messageMCPContext(message)
  const [copying, setCopying] = useState(false)
  const [failed, setFailed] = useState(false)
  async function copy() {
    setCopying(true); setFailed(false)
    try { await navigator.clipboard.writeText(context); toast.success('메일 참조를 복사했습니다. 메일 본문과 인증 정보는 포함하지 않습니다.'); onOpenChange(false) }
    catch { setFailed(true) }
    finally { setCopying(false) }
  }
  return <Dialog.Root open={open} onOpenChange={onOpenChange}><Dialog.Trigger asChild><Button size="sm" variant="ghost"><Copy size={16}/>MCP Context</Button></Dialog.Trigger><Dialog.Portal><Dialog.Overlay className="dialog-overlay"/><Dialog.Content className="notifications-dialog"><div className="row between"><Dialog.Title>MCP에서 이 메일 이어서 작업</Dialog.Title><Dialog.Close asChild><Button size="icon" variant="ghost" aria-label="MCP Context 닫기"><X size={17}/></Button></Dialog.Close></div><Dialog.Description className="muted">메일·계정·대화 ID와 도구 호출 예시만 복사합니다. 본문·제목·주소·첨부 내용·인증 키는 포함하지 않습니다. 연결한 MCP 사용자에게 이 메일을 조회할 권한이 있어야 하며, 발송에는 별도 승인이 필요합니다.</Dialog.Description><textarea aria-label="MCP 메일 참조 JSON" className="context-reference" readOnly rows={15} value={context} onFocus={event => event.target.select()}/>{failed && <p role="alert">클립보드 접근이 허용되지 않습니다. 위 텍스트를 선택하여 직접 복사해 주세요.</p>}<Button disabled={copying} onClick={copy}><Copy size={16}/>{copying ? '복사 중…' : '참조 JSON 복사'}</Button></Dialog.Content></Dialog.Portal></Dialog.Root>
}
