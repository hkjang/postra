import {useQuery} from '@tanstack/react-query'
import {Link} from 'react-router-dom'
import {toast} from 'sonner'
import {z} from 'zod'
import {api} from '@/api/client'
import {nullableList, parseResponse} from '@/api/response'
import {useSession} from '@/app/session'
import {Badge, Button, ErrorState, Loading, Panel} from '@/components/ui'
import './mcp.css'

// These are server-provided public discovery addresses, never credentials or
// values inferred from the browser origin (which may differ behind a proxy).
const publicURL = z.string().refine(value => {
  if (!value) return true
  try {
    const url = new URL(value)
    return ['http:', 'https:'].includes(url.protocol) && !url.username && !url.password && !url.search && !url.hash && value === value.trim()
  } catch { return false }
})
const endpoint = z.string().refine(value => value.startsWith('/') && !value.startsWith('//') && !/[?#\\\s]/.test(value))
const connectionSchema = z.object({
  active_endpoint: endpoint,
  configured_endpoint: endpoint,
  pending_restart: z.boolean(),
  oauth: z.object({
    enabled: z.boolean(), configured: z.boolean(), issuer: publicURL, resource_url: publicURL, metadata_url: publicURL,
    allowed_client_ids: nullableList(z.string()), scopes_supported: nullableList(z.string()),
    authorization_server: publicURL,
    proxy: z.object({
      enabled: z.boolean(), configured: z.boolean(), issuer: publicURL, metadata_url: publicURL, registration_endpoint: publicURL,
      authorization_endpoint: publicURL, token_endpoint: publicURL, callback_url: publicURL, client_id: z.string(),
    }),
  }),
})
export const parseMCPConnection = (value: unknown) => parseResponse(connectionSchema, value)

export function MCPConnectionPanel() {
  const principal = useSession()
  const query = useQuery({queryKey: ['mcp-connection'], queryFn: async ({signal}) => parseMCPConnection(await api<unknown>('/api/mcp/connection', {signal}))})
  const value = query.data
  async function copyURL(url: string) {
    try { await navigator.clipboard.writeText(url); toast.success('공개 연결 주소를 복사했습니다.') }
    catch { toast.error('주소를 선택하여 직접 복사하세요.') }
  }
  return <Panel className="mcp-connection stack">
    <div className="mcp-connection-title"><h2>MCP 연결 방법</h2><Button variant="outline" size="sm" disabled={query.isFetching} onClick={() => void query.refetch()}>연결 정보 새로고침</Button></div>
    {query.error ? <ErrorState error={query.error} retry={() => query.refetch()}/> : !value ? <Loading/> : <>
      <div className="mcp-connection-title"><Badge variant={value.oauth.enabled && value.oauth.configured ? 'secondary' : 'outline'}>{!value.oauth.enabled ? 'OAuth 사용 안 함' : value.oauth.configured ? 'OAuth 설정됨 · 연결 미확인' : 'OAuth 설정 필요'}</Badge>{value.oauth.enabled && value.oauth.proxy.enabled && <Badge variant={value.oauth.proxy.configured ? 'secondary' : 'outline'}>{value.oauth.proxy.configured ? 'DCR 프록시 사용 중' : 'DCR 프록시 설정 필요'}</Badge>}{value.pending_restart && <Badge variant="destructive">Endpoint 재시작 대기</Badge>}</div>
      <p className="muted">표시된 설정 상태는 실제 연결 성공을 의미하지 않습니다. MCP 활성화·HTTP 허용 여부, 사용자 역할과 조직 정책도 적용됩니다.</p>
      <dl className="mcp-connection-data"><dt>현재 실행 Endpoint</dt><dd><code>{value.active_endpoint}</code></dd>
        {value.pending_restart && <><dt>재시작 후 Endpoint</dt><dd><code>{value.configured_endpoint}</code><p className="small muted">재시작 전에는 현재 실행 주소를 사용하세요.</p></dd></>}
        <dt>OAuth Resource URL</dt><dd>{value.oauth.resource_url ? <><code>{value.oauth.resource_url}</code><Button size="sm" variant="outline" onClick={() => void copyURL(value.oauth.resource_url)}>Resource URL 복사</Button></> : '미설정 · 관리자에게 연결 주소를 확인하세요.'}</dd>
        <dt>Keycloak Issuer</dt><dd>{value.oauth.issuer ? <code>{value.oauth.issuer}</code> : '미설정'}<p className="small muted">조직의 인증 및 SSO Issuer 설정을 사용합니다.</p></dd>
        <dt>Resource Metadata</dt><dd>{value.oauth.metadata_url ? <a href={value.oauth.metadata_url} target="_blank" rel="noopener noreferrer">{value.oauth.metadata_url}</a> : '미제공'}</dd>
        <dt>인증 서버 안내</dt><dd>{value.oauth.authorization_server ? <><code>{value.oauth.authorization_server}</code><p className="small muted">{value.oauth.authorization_server === value.oauth.proxy.issuer && value.oauth.proxy.configured && value.oauth.proxy.enabled ? 'Postra DCR 프록시가 인증 서버로 안내되며 Keycloak 로그인을 대행합니다.' : 'Keycloak이 인증 서버로 직접 안내됩니다.'}</p></> : '미설정'}</dd>
        {value.oauth.proxy.enabled && <><dt>DCR 등록 Endpoint</dt><dd>{value.oauth.proxy.registration_endpoint ? <code>{value.oauth.proxy.registration_endpoint}</code> : '미설정'}<p className="small muted">동적 등록만 지원하는 클라이언트는 MCP URL만 입력하면 여기에 자동 등록하고 Keycloak 로그인으로 연결합니다.</p></dd>
          <dt>프록시 Keycloak Client</dt><dd>{value.oauth.proxy.client_id ? <code>{value.oauth.proxy.client_id}</code> : '미설정'}{value.oauth.proxy.callback_url && <p className="small muted">Keycloak의 이 client에 Valid Redirect URI <code>{value.oauth.proxy.callback_url}</code>를 등록해야 합니다.</p>}</dd></>}
        <dt>허용 MCP Client ID</dt><dd>{value.oauth.allowed_client_ids.length ? <ul>{value.oauth.allowed_client_ids.map((client, index) => <li key={`${index}:${client}`}><code>{client}</code></li>)}</ul> : '허용된 클라이언트 없음'}</dd>
        <dt>OAuth 허용 Scope</dt><dd>{value.oauth.scopes_supported.length ? <ul>{value.oauth.scopes_supported.map((scope, index) => <li key={`${index}:${scope}`}><code>{scope}</code></li>)}</ul> : '허용된 권한 없음'}</dd>
      </dl>
      <details open={value.oauth.enabled || undefined}><summary>Keycloak OAuth 연결 준비</summary><ol>
        <li>먼저 Postra에 SSO로 한 번 로그인하여 Keycloak 사용자와 Postra 사용자를 연결하세요.</li>
        <li>관리자가 Keycloak에 MCP 클라이언트를 사전 등록하고, 이 화면의 허용 Client ID와 Scope에 맞게 설정해야 합니다.</li>
        <li>MCP 클라이언트에서 Authorization Code + PKCE S256을 사용하세요. 클라이언트 자신의 Callback URL을 Keycloak에 등록해야 하며, Postra 웹 SSO Callback URL과는 다릅니다.</li>
        <li>클라이언트가 요청한 Scope, 토큰에 부여된 Scope, 조직 정책을 모두 만족해야 도구를 사용할 수 있습니다. 메일 발송은 별도 승인이 필요합니다.</li>
        {value.oauth.proxy.enabled && value.oauth.proxy.configured && <li>동적 클라이언트 등록(DCR)만 지원하는 클라이언트(claude.ai 커넥터, 데스크톱 앱 등)는 Client ID 없이 MCP URL만 입력하세요. Postra가 등록을 받아 Keycloak 로그인으로 연결하며, 발급된 토큰의 권한 검사는 동일합니다.</li>}
      </ol><p className="small muted">이 화면에서는 OAuth Access Token·Refresh Token이나 Client Secret을 입력·저장하지 않습니다. 토큰을 대화나 설정 예제에 붙이지 마세요.</p></details>
    </>}
    <p>기존 API Key 연결도 계속 사용할 수 있습니다. 아래에서 발급한 개인 키를 클라이언트의 Bearer 인증에 설정하세요. 로컬 stdio는 <code>postra mcp</code>를 사용합니다.</p>
    {principal?.role === 'admin' && <div><Button asChild variant="outline"><Link to="/admin?category=mcp">관리자 MCP 설정 열기</Link></Button></div>}
  </Panel>
}
