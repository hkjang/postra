import {Button, ErrorState} from '@/components/ui'
import type {MessageView} from './types'

export function ReceivedImages({body, busy, error, once, trust}: {body?: MessageView['body']; busy: boolean; error?: Error | null; once: () => void; trust: (scope: 'sender' | 'domain', revoke: boolean) => void}) {
  if (!body?.external_images) return null
  const policy = body.image_policy ?? 'block'
  const confirmTrust = (scope: 'sender' | 'domain', revoke: boolean) => {
    if (revoke || window.confirm('외부 이미지 요청으로 읽음 여부와 IP가 발신자에게 알려질 수 있습니다. 발신자 주소는 위조될 수 있으므로 신뢰할 수 있는 메일에만 허용하세요. 앞으로도 허용하시겠습니까?')) trust(scope,revoke)
  }
  return <section className="message-attachments" aria-label="외부 이미지 보호">
    <p className="small muted">{body.images_allowed ? '허용한 외부 이미지를 표시합니다.' : `개인정보 보호를 위해 외부 이미지 ${body.external_images}개를 차단했습니다.`} {policy === 'block' ? '관리자 정책에 따라 외부 이미지를 불러올 수 없습니다.' : '이미지를 불러오면 발신자에게 읽음 여부와 IP가 알려질 수 있습니다.'}</p>
    <div className="row wrap">
      {policy !== 'block' && !body.images_allowed && <Button size="sm" variant="outline" disabled={busy} onClick={once}>이번만 이미지 표시</Button>}
      {(policy === 'allow_sender' || policy === 'allow_domain') && <Button size="sm" variant="ghost" disabled={busy} onClick={()=>confirmTrust('sender',!!body.image_sender_trusted)}>{body.image_sender_trusted ? '발신자 이미지 허용 취소' : '이 발신자 이미지 항상 허용'}</Button>}
      {policy === 'allow_domain' && <Button size="sm" variant="ghost" disabled={busy} onClick={()=>confirmTrust('domain',!!body.image_domain_trusted)}>{body.image_domain_trusted ? '도메인 이미지 허용 취소' : '이 도메인 이미지 항상 허용'}</Button>}
    </div>{error && <ErrorState error={error}/>}
  </section>
}
