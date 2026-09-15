import { useMutation, useQuery } from '@tanstack/react-query'
import { Send } from 'lucide-react'
import { toast } from 'sonner'
import { api } from '@/api/client'
import { Button } from '@/components/ui'

// Handing this mail to another in-house service (aidev HANDOFF-STANDARD.md).
// The administrator's allow list decides which services appear; a fresh
// installation has none and this renders nothing. Pressing a button asks our
// own server for a five-minute single-use claim and opens the receiving
// service's /handoff with it — nobody downloads a file.
export type HandoffTarget = { name: string; origin: string }
export type HandoffTargets = { format: string; targets: HandoffTarget[] }
export type HandoffClaim = { claim: string; source: string; filename: string; content_type: string; bytes: number; expires_at: string }

export function handoffURL(target: HandoffTarget, claim: HandoffClaim) {
  return `${target.origin}/handoff?${new URLSearchParams({ source: claim.source, claim: claim.claim })}`
}

export function HandoffButtons({ messageID }: { messageID: string }) {
  const targets = useQuery({ queryKey: ['handoff-targets'], queryFn: ({ signal }) => api<HandoffTargets>('/api/handoff/targets', { signal }), staleTime: 5 * 60_000 })
  const send = useMutation({
    mutationFn: async (target: HandoffTarget) => {
      // Open the window inside the click so popup blockers let it through,
      // then point it at the receiving service once the claim exists. The
      // new window gets no handle back to this one.
      const win = window.open('about:blank', '_blank')
      if (win) win.opener = null
      try {
        const claim = await api<HandoffClaim>('/api/handoff/claims', { method: 'POST', body: { resource: messageID, format: 'markdown' } })
        const url = handoffURL(target, claim)
        if (win) win.location.replace(url); else window.open(url, '_blank', 'noopener')
        return target
      } catch (error) { win?.close(); throw error }
    },
    onSuccess: target => toast.success(`${target.name}(으)로 보냈습니다. 새 창에서 이어서 작업하세요.`),
    onError: error => toast.error(error instanceof Error ? error.message : '다른 서비스로 보내지 못했습니다.'),
  })
  const list = Array.isArray(targets.data?.targets) ? targets.data.targets : []
  if (!list.length) return null
  return <div className="message-attachments handoff-targets"><span className="muted small">다른 서비스로 보내기:</span>{list.map(target =>
    <Button key={target.origin} size="sm" variant="ghost" disabled={send.isPending} onClick={() => send.mutate(target)} title={`${target.name} 에서 이 메일을 마크다운 문서로 엽니다`}><Send size={15}/>{target.name}</Button>)}</div>
}
