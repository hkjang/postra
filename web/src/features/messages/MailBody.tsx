// Incoming and outgoing HTML is sanitized by the Go application. An opaque
// sandbox and a restrictive document CSP provide independent browser isolation.
import {MailText} from './MailText'
export function mailDocument(html: string, imagesAllowed = false): string {
  return `<!doctype html><html lang="ko"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1"><meta name="referrer" content="no-referrer"><meta http-equiv="Content-Security-Policy" content="default-src 'none'; img-src ${imagesAllowed ? 'https: http:' : "'none'"}; style-src 'unsafe-inline'; base-uri 'none'; form-action 'none'"><style>body{margin:0;padding:20px;background:white;color:#172033;font:15px/1.7 Arial,sans-serif;overflow-wrap:anywhere}table,img{max-width:100%}a{color:#3157d5}blockquote{border-left:3px solid #dfe5ee;margin-left:0;padding-left:16px}pre{white-space:pre-wrap}</style></head><body>${html}</body></html>`
}

export function MailBody({ html, text, title = '메일 본문', imagesAllowed = false, receivedMessageID, allowImagesOnce = false }: { html?: string; text?: string; title?: string; imagesAllowed?: boolean; receivedMessageID?: string; allowImagesOnce?: boolean }) {
  if (html && receivedMessageID) return <iframe className="mail-body-frame" title={title} sandbox="" referrerPolicy="no-referrer" src={`/api/messages/${encodeURIComponent(receivedMessageID)}/body/frame${allowImagesOnce ? '?external_images=once' : ''}`} />
  return html ? <iframe className="mail-body-frame" title={title} sandbox="" referrerPolicy="no-referrer" srcDoc={mailDocument(html, imagesAllowed)} /> : <MailText text={text || '본문이 없습니다.'}/>
}
