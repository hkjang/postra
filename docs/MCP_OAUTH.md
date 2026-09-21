# Keycloak OAuth로 원격 MCP 연결하기

Postra의 HTTP MCP는 기존 API Key에 더해 Keycloak의 사용자용 OAuth access token을 받을 수 있다. 이 기능은 기본적으로 꺼져 있으며 관리자가 명시적으로 활성화한다. Keycloak이 로그인과 토큰 발급을 담당하고 Postra는 `/mcp`의 Resource Server로서 토큰과 메일 접근 권한을 확인한다.

연결 방식은 두 가지다.

- **사전 등록 방식(기본)**: MCP 클라이언트가 Keycloak에 사전 등록된 자기 client ID로 Keycloak에 직접 로그인한다. 1~4절이 이 방식이다.
- **DCR 호환 프록시(선택, v0.23.0)**: 동적 클라이언트 등록(RFC 7591)만 지원하는 클라이언트(claude.ai 커넥터, 일부 데스크톱·IDE 앱)를 위해 Postra가 클라이언트에게는 OAuth Authorization Server로 보이고, 실제 로그인은 사전 등록한 하나의 프록시용 Keycloak client로 대행한다. 5절을 참고한다. 두 방식은 함께 켤 수 있으며 발급된 토큰의 검증과 권한 계산은 동일하다.

새 OAuth 연결은 **HTTP MCP 전용**이다. REST API의 인증 방법을 확장하거나 브라우저 SSO 세션 쿠키를 MCP 자격 증명으로 바꾸지 않는다. 로컬 `postra mcp`의 stdio 방식과 기존 API Key 방식도 유지한다. 메일 조회·작성·승인·발송의 공통 권한 계약은 [MCP_CONVERGENCE.md](MCP_CONVERGENCE.md)를 참고한다.

## 먼저 확인할 사항

- 운영에서는 Postra의 인증을 활성화한다(`POSTRA_AUTH_ENABLED=true`, 변경 시 재시작). 인증을 끈 신뢰 로컬 모드의 REST API까지 OAuth MCP 설정이 보호하는 것은 아니다.
- 기존 Keycloak SSO가 `/app/login`에서 정상 작동해야 한다. OAuth MCP도 같은 `auth.oidc.issuer`를 신뢰한다.
- 사용자는 **브라우저에서 Postra에 SSO 로그인하여 연결된 Postra 사용자가 먼저 존재**해야 하며, 해당 사용자가 활성 상태여야 한다. MCP Bearer token만으로 사용자를 생성하거나 이메일이 같은 계정을 자동 연결하지 않는다.
- 사전 등록 방식에서는 MCP 클라이언트가 OAuth Authorization Code + PKCE와 **사전 등록한 client ID 지정**을 지원해야 한다. client ID를 직접 지정할 수 없고 DCR만 지원하는 클라이언트는 5절의 DCR 호환 프록시를 사용한다. CIMD(client ID metadata document)는 지원하지 않는다.
- 운영 주소는 브라우저·MCP 클라이언트·Postra가 접근할 수 있는 내부 HTTPS 주소로 정한다. 내부 CA도 각 실행 환경에서 신뢰해야 한다.
- 관리자는 설정을 바꾸기 전에 현재 로그인 세션과 복구용 자격 증명을 확보한다. API Key를 먼저 폐기할 필요는 없다.

MCP 2026-07-28은 사전 등록을 지원하며, 이미 등록 정보를 가진 클라이언트는 이를 우선 사용할 수 있다. DCR은 호환용으로 deprecated이지만 현재 배포된 다수의 클라이언트가 여전히 DCR만 사용한다. Keycloak의 CIMD는 별도 실험적 기능이며 metadata URL 접근을 요구하므로 폐쇄망에서는 사전 등록 또는 Postra DCR 프록시를 권장한다. [MCP Client Registration](https://modelcontextprotocol.io/specification/2026-07-28/basic/authorization/client-registration), [Keycloak MCP 가이드](https://www.keycloak.org/securing-apps/mcp-authz-server)

## 1. 주소와 클라이언트 분리

예시 운영값은 다음과 같다. 실제 배포 주소와 클라이언트 이름으로 바꾼다.

| 용도 | 예시 |
| --- | --- |
| Keycloak issuer | `https://sso.corp.local/realms/corp` |
| 기존 Postra 웹 SSO client ID | `postra-web` |
| 새 MCP client ID | `postra-mcp-desktop` |
| Postra MCP의 canonical resource URL | `https://postra.corp.local/mcp` |
| MCP client callback | 해당 MCP 클라이언트가 안내하는 정확한 callback URL |

MCP callback은 **MCP 클라이언트가 받는 주소**다. 기존 Postra 웹 SSO callback인 `/auth/oidc/callback`을 그대로 복사하지 않는다. 데스크톱 클라이언트는 loopback callback, 서버형 클라이언트는 자신의 HTTPS callback을 사용할 수 있으므로 해당 제품의 등록 안내를 확인한다.

`resource_url`은 클라이언트가 실제 접속하는 MCP 주소와 같게 설정한다. 리버스 프록시 뒤의 `http://127.0.0.1:8480/mcp`가 아니라 외부에서 사용하는 HTTPS 주소가 기준이다. HTTPS가 기본이며 로컬 개발용 loopback 주소에만 HTTP를 허용한다. 비밀번호, query string, fragment를 넣지 않는다. 공개 URL의 경로는 **현재 실행 중인 MCP endpoint와 정확히 일치**해야 한다. 프록시에만 경로 접두사를 추가하지 않으며, 기본 endpoint `/mcp`를 바꿀 때는 Postra 설정과 공개 URL을 함께 변경하고 재시작한다. 재시작 전 경로가 불일치하는 동안 OAuth 연결은 허용되지 않는다.

## 2. Keycloak에 MCP client 사전 등록

기존 Postra 웹 SSO와 같은 realm에서 MCP client를 새로 만든다. 웹 SSO client ID를 MCP 허용 목록에 재사용하지 않는다.

1. Client type을 OpenID Connect로 설정하고 고유한 client ID를 지정한다.
2. 데스크톱 등 public client는 **Client authentication OFF**, 비밀을 안전하게 보관하는 서버형 confidential client는 **ON**으로 설정한다. confidential client secret은 그 MCP 클라이언트에만 안전하게 전달한다.
3. **Standard Flow ON**, **PKCE method S256**으로 설정한다. Direct Access Grants, Implicit Flow와 Service account roles는 이 사용자 위임용 설정에서 끈다.
4. Valid Redirect URIs에 해당 MCP 클라이언트의 callback을 정확히 등록한다. 운영에서 전체 허용 `*`를 사용하지 않는다. 동적 loopback 포트를 사용하는 제품은 해당 Keycloak 버전의 loopback 정책과 제품 안내를 별도로 확인한다.
5. Public client의 refresh token은 rotation 정책을 사용한다. 초기 운영에서는 access-token 수명을 **5분 정도의 짧은 값**으로 설정하는 것을 권장한다.

빈 PKCE 설정은 S256 강제를 뜻하지 않는다. Keycloak의 설정 이름·위치는 버전에 따라 다를 수 있다. [Keycloak Client 설정·토큰 수명](https://www.keycloak.org/docs/latest/server_admin/), [MCP OAuth 보안](https://modelcontextprotocol.io/specification/2026-07-28/basic/authorization/security-considerations)

### Scope와 audience mapper

처음에는 `mail.read`, `mail.search` 두 개의 OIDC client scope를 만들고 새 MCP client에 **Optional**로 연결한다. 각 scope의 **Include in token scope**를 켜 실제 access token의 `scope`에 이름이 포함되게 한다.

각 scope에 Audience mapper를 추가한다.

| Mapper 항목 | 값 |
| --- | --- |
| Mapper type | Audience |
| Included Custom Audience | `https://postra.corp.local/mcp` |
| Add to access token | ON |
| Add to ID token | OFF |

Audience는 client ID `postra-mcp-desktop`이나 웹 SSO client ID가 아니라 **Postra MCP의 canonical resource URL**이다. 토큰의 `azp`는 반대로 토큰을 발급받은 MCP client ID다. 두 값의 역할을 바꾸지 않는다.

MCP 클라이언트는 authorization 및 token 요청에 `resource`를 전달해야 한다. 다만 Keycloak 버전·활성화 기능에 따라 이를 직접 처리하는 범위가 다르므로 `resource`만 보내면 audience가 자동 설정된다고 가정하지 않는다. Keycloak 공식 MCP 가이드는 optional scope의 custom audience mapper 방식을 안내한다. [Keycloak MCP audience 구성](https://www.keycloak.org/securing-apps/mcp-authz-server)

Postra의 DCR 프록시는 authorization·token 요청 모두에 `resource`를 Keycloak으로 그대로 전달한다(RFC 8707). Resource Indicator를 처리하는 Keycloak 버전에서는 이것만으로 audience가 채워지고, 모르는 버전은 파라미터를 무시하므로 Audience mapper 구성이 여전히 기준이다. 클라이언트가 보낸 `resource`는 scheme·host 대소문자와 끝의 `/`만 다른 경우 같은 값으로 취급한다.

## 3. Postra 관리자 화면에서 활성화

1. `/app/admin`의 **인증 및 SSO**에서 기존 issuer·웹 client·callback을 확인한다. OIDC 연결 시험은 discovery 확인이며 실제 사용자 로그인 성공을 대신하지 않는다.
2. **MCP** 카테고리로 이동한다. 화면 검색에 아래 설정 키를 그대로 입력해도 된다.
3. MCP/HTTP MCP 활성 상태를 확인하고 OAuth 주소·client·scope를 입력한 뒤 변경사항을 검토한다.
4. `mcp.oauth.enabled`를 켜 저장한다. OAuth 설정은 다음 요청부터 적용된다. 기존 `mcp.endpoint` 변경은 재시작 항목이므로 현재 실행 경로와 저장된 변경 대기를 구분한다.

| 설정 키 | 초기값 | 입력 예시·의미 |
| --- | --- | --- |
| `mcp.oauth.enabled` | `false` | Keycloak OAuth MCP 사용 여부 |
| `mcp.oauth.resource_url` | 미설정 | `https://postra.corp.local/mcp` |
| `mcp.oauth.allowed_client_ids` | 미설정 | `postra-mcp-desktop,postra-mcp-agent` — 쉼표 구분, 웹 SSO client와 별도 |
| `mcp.oauth.allowed_scopes` | `mail.read,mail.search` | OAuth로 허용할 최대 권한 범위, 쉼표 구분 |
| `auth.oidc.issuer` | 기존 SSO 설정 | 신뢰할 Keycloak realm issuer |

Scope 이름은 `mail.read`, `mail.search`, `mail.ai`, `mail.draft`, `mail.send`, `mail.delete`, `mail.work`, `admin.read`, `admin.write`다. `mail.*` 같은 wildcard는 사용하지 않는다. 이 목록은 Postra 전체 scope 이름이지 처음부터 모두 부여하라는 의미가 아니다.

유효 권한은 **토큰 scope ∩ OAuth 허용 scope ∩ 조직 MCP 권한 ∩ 사용자 역할·도구 정책 ∩ 데이터 소유권 ∩ 작업별 승인**이다. `mcp.oauth.allowed_scopes`에 추가한다고 Keycloak token에 권한이 생기지는 않는다. 반대로 Keycloak에서 허용한 scope라도 Postra 정책이 차단하면 실행할 수 없다. 일반 사용자가 관리자 scope를 요청하거나 토큰에 관리자 역할이 있다고 해서 Postra 관리자 권한을 얻지 않는다.

## 4. 사용자 연결과 확인

1. 사용자가 브라우저에서 Postra에 **Keycloak SSO로 먼저 로그인**한다. 같은 이메일의 로컬 사용자만 만들거나 로컬 비밀번호로만 로그인한 것은 OIDC 연결을 대신하지 않는다.
2. MCP 클라이언트에 Postra MCP URL과 사전 등록된 client ID를 설정한다. confidential client만 해당 client secret을 사용한다.
3. OAuth 연결을 시작하여 Keycloak에서 로그인한다. 기본 요청 scope는 `mail.read mail.search`다. 이미 Keycloak 세션이 있으면 자격 증명 재입력이 생략될 수 있다.
4. `mail_identity`와 읽기·검색 도구로 올바른 Postra 사용자와 자신의 메일만 보이는지 확인한다.
5. AI·작성·발송이 필요하면 관리자와 함께 Keycloak scope, Postra OAuth 허용 scope, 조직 정책을 각각 확장하고 다시 인증한다. 메일 발송 승인은 OAuth에서도 생략되지 않는다.

Bearer token은 MCP 클라이언트가 매 HTTP 요청의 `Authorization` 헤더로 보낸다. URL query, 채팅 메시지, 메일 본문에 넣지 않는다. Postra의 관리자 화면은 access token이나 refresh token을 입력·보관하는 장소가 아니며, 연결을 위해 사용자가 토큰을 수동 복사할 필요가 없는 클라이언트 사용을 권장한다.

### Discovery 주소

기본 endpoint가 `/mcp`라면 공개 metadata는 다음 두 주소에서 제공한다.

```text
https://postra.corp.local/.well-known/oauth-protected-resource/mcp
https://postra.corp.local/.well-known/oauth-protected-resource
```

Endpoint가 `/tools/mcp`이면 경로 포함 주소는 `/.well-known/oauth-protected-resource/tools/mcp`다. 프록시가 MCP 경로뿐 아니라 metadata 경로도 전달해야 한다. 미인증 요청의 `WWW-Authenticate`에 표시된 `resource_metadata`가 discovery 진입점이며, metadata의 `authorization_servers`는 Keycloak issuer를 가리킨다(5절의 DCR 프록시를 켜면 Postra origin을 가리킨다). Keycloak의 `/.well-known/openid-configuration`을 Postra가 대신 발급하는 구조가 아니다. [MCP Discovery](https://modelcontextprotocol.io/specification/2026-07-28/basic/authorization/authorization-server-discovery)

## 5. DCR 호환 OAuth 프록시 (선택)

DCR만 지원하는 클라이언트는 MCP URL만 입력하면 Protected Resource Metadata에서 인증 서버를 찾아 `registration_endpoint`에 스스로 등록하고, 등록으로 받은 client ID로 로그인을 시작한다. Keycloak의 익명 DCR을 열지 않고 이를 지원하기 위해 Postra가 다음 역할을 맡는다.

| 클라이언트가 보는 것 | Postra 동작 |
| --- | --- |
| `https://postra.corp.local/.well-known/oauth-authorization-server` | RFC 8414 메타데이터. issuer는 공개 MCP URL의 origin, PKCE S256·`authorization_code`·`refresh_token`만 지원 |
| `POST /oauth/register` | RFC 7591 등록. redirect URI는 HTTPS, loopback HTTP(포트 자유), 또는 네이티브 앱 스킴만 허용. public(`none`) 또는 secret 발급(`client_secret_basic`/`client_secret_post`) |
| `GET /oauth/authorize` | 클라이언트·redirect URI·PKCE·scope 검사 후 **사용자 동의 화면**을 보여 주고, 허용 시 프록시용 Keycloak client로 Keycloak 로그인으로 이동 |
| `GET /oauth/callback` | Keycloak 콜백. Keycloak code를 Postra가 봉인한 code로 감싸 클라이언트의 redirect URI로 전달 |
| `POST /oauth/token` | 클라이언트의 PKCE verifier·client 인증을 먼저 검증한 뒤에만 Keycloak token endpoint를 호출. access token은 Keycloak이 발급한 그대로, refresh token은 해당 클라이언트에 묶어 봉인해 반환 |

access token은 Keycloak의 Bearer JWT 그대로이므로 `/mcp`의 검증(issuer·audience·`azp`·scope·사용자 연결)은 사전 등록 방식과 완전히 같다. `azp`는 프록시용 client ID가 되며 Postra가 자동으로 허용 목록에 포함한다. 봉인된 code·state·refresh token은 복제본이 공유하는 상태 키로 암호화되므로 다중 replica 배포에서도 DB에 별도 세션을 두지 않고 동작한다.

### Keycloak에 프록시용 client 등록

같은 realm에 **웹 SSO client·기존 MCP client와 다른** client를 하나 더 만든다. 예: `postra-mcp-proxy`.

1. Standard Flow ON, PKCE S256, Direct Access Grants·Implicit·Service account OFF.
2. Valid Redirect URIs에 **`https://postra.corp.local/oauth/callback`** 하나만 정확히 등록한다(공개 MCP URL의 origin + `/oauth/callback`). 클라이언트 앱의 loopback·`claude.ai` 콜백은 Keycloak이 아니라 Postra가 검증하므로 Keycloak에 넣지 않는다.
3. 2절과 같은 `mail.*` optional scope와 공개 MCP URL Audience mapper를 이 client에도 연결한다. Postra는 사용자가 허용한 scope에 `openid`를 더해 요청한다.
4. Postra가 서버 측에서 코드를 교환하므로 **Client authentication ON(confidential)**을 권장한다. 이때 client secret을 Postra 관리자 설정의 `OAuth 프록시 Client Secret`에 입력한다. public client로 두면 secret 없이 동작한다.
5. Consent Required는 켜지 않아도 된다. Postra가 클라이언트마다 자체 동의 화면을 보여 준다(아래 참조). 짧은 access-token 수명 권장은 동일하다.

### Postra 설정

| 설정 키 | 초기값 | 의미 |
| --- | --- | --- |
| `mcp.oauth.proxy.enabled` | `false` | DCR 호환 프록시 사용 여부. `mcp.oauth.enabled`가 켜져 있어야 한다 |
| `mcp.oauth.proxy.client_id` | 미설정 | 프록시용 Keycloak client ID. 웹 SSO client와 달라야 하며 공백·쉼표 없는 하나의 값 |
| `mcp.oauth.proxy.client_secret_ref` | 미설정 | 쓰기 전용 비밀값. confidential client일 때만 입력하며 SecretStore에 암호화 저장된다 |

켜면 Protected Resource Metadata의 `authorization_servers`가 Keycloak 대신 Postra origin을 가리킨다. 사전 등록 client ID를 지정하는 클라이언트도 계속 동작한다. 허용 목록의 client ID로 `/oauth/authorize`·`/oauth/token`에 오면 Postra는 요청을 그대로 Keycloak으로 넘기고(pass-through) redirect URI·PKCE 검증은 Keycloak이 한다. 개인 MCP 화면과 관리자 MCP 화면에서 등록 endpoint, 프록시 client, Keycloak에 넣을 콜백 주소를 확인할 수 있다.

### 사용자 동의 화면

모든 DCR 클라이언트가 하나의 Keycloak client를 공유하므로 Keycloak은 어느 클라이언트가 요청했는지 구분하지 못한다. 이미 Keycloak 세션이 있는 사용자를 공격자가 만든 `/oauth/authorize` 링크로 유도하면 로그인 없이 공격자의 콜백으로 토큰이 흘러갈 수 있다(MCP 사양의 confused deputy 문제). 이를 막기 위해 Postra는 Keycloak으로 보내기 전에 **클라이언트 이름·client ID·콜백 주소·요청 권한**을 보여 주고 허용/거부를 받는다.

- 동의는 같은 브라우저의 일회용 쿠키와 봉인된 요청에 함께 묶이며, 다른 사이트에서 제출한 POST는 거부한다.
- 사용자는 직접 시작한 연결이 아니거나 콜백 주소를 모르면 **거부**해야 한다. 거부하면 클라이언트에 `access_denied`가 전달된다.
- 클라이언트마다 동의를 기억하지 않는다. 재인증할 때마다 화면이 다시 표시된다.

### 등록 관리와 한계

- 등록은 익명이며 로그인 권한을 부여하지 않는다. 등록(분당 60)·로그인 시작·콜백(분당 300)·토큰 요청(분당 600)은 접속 주소별로 제한한다. 리버스 프록시 뒤에서는 접속 주소가 프록시 하나이므로 이 제한은 폭주 방어일 뿐 세밀한 사용자 제한이 아니다. 30일간 사용하지 않은 등록은 다음 등록 시 정리하며 총 500개를 넘으면 등록을 거부한다.
- 클라이언트가 요청한 scope 중 Postra 허용 범위 밖의 값은 오류 대신 제외하고, 실제 부여된 scope를 token 응답의 `scope`로 알려 준다. 요청에 사용할 수 있는 scope가 하나도 없으면 기본값 `mail.read mail.search`를 적용한다.
- 관리자는 `/app/admin`의 MCP 화면(또는 `GET /api/admin/mcp-oauth-clients`)에서 등록된 클라이언트와 콜백 주소를 확인하고 삭제할 수 있다. 삭제하면 그 클라이언트의 새 로그인과 refresh가 즉시 중단되지만, 이미 발급된 access token은 Keycloak 만료까지 유효하다.
- Postra의 refresh token은 Keycloak refresh token을 봉인한 것으로 클라이언트 밖에서는 쓸 수 없고 다른 클라이언트로 옮겨 쓸 수 없다. Keycloak의 refresh token rotation 정책은 그대로 적용된다.
- token endpoint는 Keycloak이 발급한 access token을 `/mcp`와 같은 기준으로 먼저 검증하고, Audience mapper 누락·사용자 미연결 등으로 거부되면 클라이언트에 `invalid_grant`와 점검 항목을 안내하며 관리자 장애 화면에 기록한다.
- 지원 범위는 Authorization Code + PKCE S256, `refresh_token`, `none`/`client_secret_basic`/`client_secret_post` 인증이다. CIMD, `client_credentials`, token introspection·revocation endpoint, DPoP는 제공하지 않는다. 브라우저 페이지에서 직접 실행되는 MCP 클라이언트의 교차 출처 token 요청은 기존 정책대로 허용하지 않는다.
- 별도 MCP listener(`POSTRA_MCP_HTTP_ADDR`)를 쓰는 배포에서는 그 listener가 `/oauth/*`와 `/.well-known/oauth-authorization-server`도 함께 제공하므로 프록시는 두 주소 모두 전달해야 한다.

### 클라이언트 연결 절차

1. 사용자가 브라우저에서 Postra에 SSO로 먼저 로그인해 사용자 연결을 완료한다.
2. MCP 클라이언트에 Postra MCP URL(`https://postra.corp.local/mcp`)만 입력한다. client ID·secret은 비워 둔다.
3. 클라이언트가 자동 등록 후 브라우저를 열면 Postra 동의 화면에서 클라이언트와 콜백 주소를 확인하고 허용한다. 이어서 Keycloak 로그인이 진행된다.
4. `mail_identity`로 올바른 사용자·scope인지 확인한다. 권한 확장은 4절과 같이 Keycloak scope·Postra 허용 scope·조직 정책을 함께 조정하고 재인증한다.

## 검증·폐기 동작과 한계

Postra는 신뢰한 issuer의 키로 서명, issuer, resource audience, 만료와 유효 시작 시각, 허용 client ID(`azp`), access-token 종류와 scope를 확인한다. **ID token은 MCP access token으로 사용할 수 없다.** Keycloak의 payload `typ: Bearer`와 JOSE header `typ: JWT`는 서로 다른 필드다. 이 연동은 Keycloak Bearer JWT를 대상으로 하며 모든 OAuth 제공자·opaque token·DPoP client와의 호환을 의미하지 않는다. [Keycloak TokenVerifier](https://github.com/keycloak/keycloak/blob/26.7.3/core/src/main/java/org/keycloak/TokenVerifier.java)

Keycloak discovery의 `jwks_uri`는 설정한 issuer와 동일한 scheme·host·port를 사용해야 하며 리다이렉트를 따르지 않는다. JWKS를 별도 도메인/CDN으로 옮긴 구성은 이 제한과 맞지 않는다. 서명은 RSA·ECDSA·RSA-PSS 계열 허용 목록으로 제한하고 HMAC·무서명 토큰은 받지 않는다. 메타데이터 응답 크기와 요청 시간을 제한하며, 알려지지 않은 서명 키로 인한 반복 조회는 제한한다. 키 교체 직후에는 최대 약 1초의 JWKS 재조회 대기가 있을 수 있다.

이 구현은 Keycloak introspection endpoint를 매 요청 호출하지 않는다. 따라서 **Keycloak에서 로그아웃하거나 사용자·client를 비활성화해도 이미 발급된 access token은 만료 시각까지 유효할 수 있다.** Keycloak 사용 중지의 즉시 반영을 보장하지 않으며, 짧은 access-token 수명을 권장하는 이유다.

반면 Postra의 **로컬 사용자 비활성화**와 OAuth 사용 중지·client 허용 목록·권한 축소는 다음 요청부터 다시 검사한다. 긴 MCP 세션이 있어도 최초 인증 권한을 계속 신뢰하지 않는다. 이미 시작된 발송이나 작업의 소급 취소를 의미하지는 않는다. 긴급 차단 시에는 Postra의 사용자 비활성화 또는 해당 MCP client 허용 제거도 함께 사용한다. API Key를 별도로 가진 사용자라면 OAuth 사용 중지만으로 그 키까지 폐기되지 않으므로 키 정책도 별도로 확인한다.

본문 1 MiB 이하의 정상적인 단일 도구·Resource 요청에서 토큰 권한이 부족하면 `403 insufficient_scope`와 필요한 scope를 안내하여 클라이언트가 추가 승인을 요청할 수 있게 한다. 관리자 정책상 허용되지 않은 scope에는 재승인 반복을 유도하지 않는다. 큰 요청이나 잘못된 프로토콜 요청은 기존 MCP SDK 경로에서 동일한 권한 검사를 받으며, 권한이 자동으로 늘어나지 않는다. 큰 첨부 작업은 먼저 필요한 초안 권한을 승인받은 뒤 진행한다.

## 폐쇄망·프록시 운영

- 인터넷 연결은 필수가 아니다. Postra와 MCP 클라이언트가 내부 Keycloak의 discovery·JWKS·로그인·token endpoint에 필요한 범위로 접근할 수 있어야 한다.
- 서버, 프록시, 사용자 PC의 DNS와 시간을 맞춘다. issuer hostname, proxy 외부 URL과 인증서 SAN이 일치해야 한다.
- 내부 CA를 운영체제·컨테이너·클라이언트의 신뢰 저장소에 설치한다. `--insecure`나 TLS 검증 해제를 운영 해결책으로 사용하지 않는다.
- 프록시는 `Authorization`, `WWW-Authenticate`, MCP session 관련 헤더를 보존하고 access-token 원문을 access log에 기록하지 않게 한다.
- Postra의 기존 교차 출처 브라우저 MCP 제한이 유지된다. 임의 웹 출처에서의 연결이나 특정 상용 클라이언트의 네트워크 제약까지 자동 해결하는 기능은 아니다.
- 외부 CIMD URL에 접속할 수 없는 클라이언트나 사전 등록 설정을 지원하지 않는 제품은 API Key 방식을 유지하거나 해당 제품의 폐쇄망 연결 방식을 확인한다. 등록 문제를 해결하려고 anonymous DCR나 임의 redirect 허용을 열지 않는다.

## 문제 해결

| 증상 | 먼저 확인할 항목 |
| --- | --- |
| OAuth 연결 선택이 없거나 등록 단계 실패 | 클라이언트의 사전 등록 client ID 지원, client 유형·callback, OAuth 활성화 여부 |
| Discovery가 404이거나 잘못된 주소 안내 | 실제 MCP endpoint, canonical resource URL, metadata 경로의 프록시 전달 |
| 401 또는 인증 실패 | issuer/서명/만료/`nbf`, audience mapper, 허용 client ID, ID token 오사용, Postra SSO 연결 사용자 존재 |
| 로그인은 됐지만 도구 권한 부족 | token scope, OAuth 허용 scope, 조직 MCP 권한, 사용자 역할의 교집합 |
| 다른 realm/client의 토큰이 동작하지 않음 | 의도된 차단이다. issuer나 audience 검사를 끄지 않고 정확한 client로 재인증 |
| Keycloak에서 끈 사용자가 잠시 계속 접근 | 이미 발급한 access token의 남은 수명 확인, 긴급하면 Postra 사용자도 비활성화 |
| 내부 CA 또는 연결 오류 | 각 실행 환경의 CA·DNS·방화벽·시간 확인; endpoint의 비밀값이나 token을 로그로 출력하지 않음 |
| Keycloak 로그인은 되지만 Postra 사용자가 연결되지 않음 | 먼저 웹 SSO 로그인, issuer·subject 연결 확인; 이메일 기반 Bearer 자동 병합은 수행하지 않음 |
| DCR 클라이언트 등록이 404 | `mcp.oauth.proxy.enabled`, `mcp.oauth.enabled`, 프록시 client ID, 리버스 프록시의 `/oauth/*`·`/.well-known/oauth-authorization-server` 전달 |
| 등록은 되지만 동의 화면 대신 오류 페이지나 `error`가 전달됨 | 클라이언트가 보낸 redirect URI가 등록한 것과 다름(loopback은 포트만 가변), PKCE S256 누락, 다른 `resource` 값, 허용 scope가 하나도 없음 |
| 동의 후 Keycloak이 `invalid_redirect_uri` | 프록시용 Keycloak client의 Valid Redirect URI에 `https://<공개 MCP origin>/oauth/callback`이 정확히 있는지 확인 |
| token endpoint가 `invalid_grant`를 반환 | `error_description`이 거부 사유를 그대로 말한다(audience, 허용 client, 미연결 사용자, ID token 등). 같은 문장이 서버 로그와 관리자 인시던트에도 남는다 |
| 연결·도구 목록은 되는데 모든 도구가 `insufficient_scope` | 토큰의 `scope`에 `mail.*`가 없다. Keycloak client scope의 **Include in token scope**와 client 연결을 확인한다. 이 경우 관리자 인시던트가 기록된다 |
| 권한이 없는 도구를 한 번 호출하면 연결이 끊김 | scope 거부는 HTTP 403이 아니라 JSON-RPC 도구 오류로 반환되며 세션이 유지된다 |
| 실패한 호출마다 클라이언트가 스키마 검증 오류를 보고 | 도구 오류 결과는 `structuredContent` 를 싣지 않는다. 오류 봉투는 text content 와 `_meta.postra_error` 로 전달된다. OAuth 토큰은 Keycloak 이 발급한 scope 만 가지므로 거부가 잦아 이 문제가 MCP 키보다 먼저 드러난다 |
| 예전에 연결해 둔 클라이언트가 갑자기 `invalid_client` | 등록은 30일 미사용 시 정리되고 관리자가 폐기할 수 있다. 클라이언트에서 연결을 삭제하고 다시 추가하면 새로 등록된다 |
| 동의 화면이 반복해서 나타남 | 의도된 동작이다. 클라이언트별 동의는 기억하지 않는다 |

지원 요청에는 시각, Postra/Keycloak/클라이언트 버전, 비밀이 없는 설정 키와 오류 코드만 포함한다. **토큰 원문, client secret, Authorization 헤더, 전체 JWT payload를 채팅·스크린샷·공개 JWT 디코더에 붙여 넣지 않는다.** 필요한 claim 점검은 승인된 내부 도구로 로컬에서 수행한다.

## 검증 범위

애플리케이션의 토큰·권한 검사와 HTTP discovery·인증 회귀 검증은 로컬 테스트용 issuer/JWKS·서명 키를 사용할 수 있다. 이러한 자동 테스트는 실제 운영 Keycloak realm이나 모든 MCP 클라이언트와의 배포 시험을 대신하지 않는다. 운영자는 위의 순서대로 브라우저 SSO → MCP OAuth → 자기 메일 조회 → 권한 거부 → 토큰 만료·로컬 사용자 비활성화를 대상 클라이언트에서 확인한 뒤 확대 적용해야 한다.

이 문서는 특정 운영 Keycloak 배포 또는 상용 MCP 클라이언트에서 연결 시험을 완료했다는 의미가 아니다.
