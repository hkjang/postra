import { useQuery } from '@tanstack/react-query'
import { APIError } from '@/api/client'
import type { DraftAttachment } from '../messages/types'
import { MailBody } from '../messages/MailBody'

export function fileBase64(file: Blob): Promise<string> {
  return new Promise((resolve, reject) => {
    const reader = new FileReader()
    reader.onerror = () => reject(new Error('파일을 읽지 못했습니다.'))
    reader.onload = () => resolve(String(reader.result).split(',')[1] || '')
    reader.readAsDataURL(file)
  })
}

export function attachmentURL(draftID: string, attachmentID: string, version: number) {
  return `/api/drafts/${encodeURIComponent(draftID)}/attachments/${encodeURIComponent(attachmentID)}?version=${version}`
}

export function inlineDocument(html: string, images: Record<string, string>): string {
  const document = new DOMParser().parseFromString(html, 'text/html')
  document.querySelectorAll('img').forEach(element => {
    const cid = element.getAttribute('src')?.replace(/^cid:/, '') || ''
    const image = images[cid]
    if (!/^data:image\/(png|jpeg|gif);base64,[A-Za-z0-9+/]+=*$/.test(image || '')) { element.remove(); return }
    // No authored image attribute can override the trusted, owner-loaded data.
    const replacement = document.createElement('img')
    replacement.src = image; replacement.alt = element.getAttribute('alt') || '본문 이미지'
    replacement.style.maxWidth = '100%'; replacement.style.height = 'auto'
    element.replaceWith(replacement)
  })
  return `<!doctype html><html lang="ko"><head><meta charset="utf-8"><meta http-equiv="Content-Security-Policy" content="default-src 'none'; img-src data:; style-src 'unsafe-inline'; base-uri 'none'; form-action 'none'"><style>body{margin:0;padding:20px;background:white;color:#172033;font:15px/1.7 Arial,sans-serif;overflow-wrap:anywhere}table{max-width:100%}pre{white-space:pre-wrap}</style></head><body>${document.body.innerHTML}</body></html>`
}

export function DraftBody({ html, text, draftID, version, attachments, title = '메일 본문' }: { html?: string; text?: string; draftID: string; version: number; attachments?: DraftAttachment[]; title?: string }) {
  const inline = (attachments || []).filter(item => item.inline && item.content_id)
  const images = useQuery({ queryKey: ['draft-inline-images', draftID, version, inline.map(item => item.id).join(',')], enabled: !!html && inline.length > 0, queryFn: async ({ signal }) => {
    const entries = await Promise.all(inline.map(async attachment => {
      const response = await fetch(attachmentURL(draftID, attachment.id, version), { credentials: 'same-origin', signal })
      if (!response.ok) { if (response.status === 401) window.dispatchEvent(new Event('postra:unauthorized')); throw new APIError('본문 이미지를 읽지 못했습니다.', response.status) }
      const blob = await response.blob()
      return [attachment.content_id!, `data:${attachment.mime_type};base64,${await fileBase64(blob)}`] as const
    }))
    return Object.fromEntries(entries)
  } })
  if (!html || !inline.length) return <MailBody html={html} text={text} title={title} />
  return <>{images.error && <p role="alert" className="preview-warning">본문 이미지가 표시되지 않았습니다. <button type="button" onClick={() => images.refetch()}>다시 불러오기</button></p>}<iframe className="mail-body-frame" title={title} sandbox="" referrerPolicy="no-referrer" srcDoc={inlineDocument(html, images.data || {})} /></>
}
