# MCP와 웹의 공통 메일 기능·권한 계약

MCP는 별도 메일 처리기를 두지 않습니다. REST와 동일한 `application.App`의 렌더러, 초안, 승인, SMTP, 동기화 작업, 사용자 소유권 검사를 호출합니다. 메일 본문·첨부·서명은 사용자별로 격리되며, 관리자 역할은 다른 사용자의 메일을 읽을 권한이 아닙니다.

## 탐색과 실행

- `mail_capabilities`, `mail_identity`, `mail_system_info`는 기능·현재 주체·비밀이 아닌 운영 정보를 제공합니다.
- MCP 표준 `tools/list`는 SDK가 생성한 입력 JSON Schema와 실제 Go 응답 DTO/도구별 래퍼에서 생성한 출력 JSON Schema, 표준 `annotations` 및 `_meta.postra`를 반환합니다. 모든 등록 도구와 별칭의 성공 응답에는 필드·타입·필수 여부가 명시됩니다. 예를 들어 승인은 `{preview, approval}`, 발송은 outbound 객체, 발송 목록은 `{outbound: [...]}`입니다. REST와 같은 nullable/embedded 필드 추론을 사용하지만 REST 전용 URL 인수 생략 규칙은 MCP에 적용하지 않습니다. 메타데이터는 `sideEffects`, `requiresApproval`, `requiredScopes`, `examples`, `errorContract`를 포함하며 일부 도구는 `conditionalScopes`도 제공합니다. 메타데이터는 안내이고, 서버의 실제 권한 검사와 승인을 대체하지 않습니다.
- 기존 도구 이름은 유지됩니다. `mail_accounts_list`, `mail_get`, `mail_thread`, `mail_draft`, `mail_preview`, `mail_approve`, `mail_systeminfo`는 대응하는 기존 도구의 별칭입니다. 별칭을 사용해도 동일한 원래 도구 정책을 검사합니다.
- 기존 `mail://` 리소스와 함께 `postra://mail/{id}`, `postra://mail/{id}/raw`, `postra://thread/{id}`, `postra://draft/{id}`, `postra://account/{id}`, `postra://action/{id}`, `postra://job/{id}`를 제공합니다. 리소스 읽기도 도구와 같은 권한 검사를 거치며 권한이 부족하면 원문을 반환하지 않습니다.
- `mail_render`는 웹과 같은 자동 감지·템플릿·서명을 사용합니다. `smart_format`은 추가 `mail.ai` 권한을 요구하며 명시적으로 요청할 때만 AI를 호출합니다. 선택 문장만 수정하는 `mail_text_rewrite`는 저장된 초안을 바꾸지 않습니다.
- 초안 목록은 `mail_drafts_list`, 현재 초안은 `mail_draft_get`, 폐기는 `mail_draft_delete`입니다. 폐기에는 `confirm=true`가 필요하고 발송된 초안은 폐기할 수 없습니다.
- 초안 첨부는 `mail_draft_attachment_add/get/remove`를 사용합니다. 추가·제거는 새 버전을 만들어 승인을 무효화합니다. MCP 파일 입력은 1 MiB로 제한하고 읽기는 기본 메타데이터/인증 REST 다운로드 경로만 반환합니다. `include_content=true`일 때만 최대 1 MiB base64 내용을 반환합니다. 큰 파일은 웹/REST 첨부 기능을 사용합니다.
- `mail_events`는 [개인 알림](NOTIFICATIONS.md)과 동일한 실제 작업·발송·액션·보안 상태 메타데이터를 반환합니다. 본문·수신자·오류 상세가 포함되지 않으며 조직/개인 알림 설정을 따릅니다.
- `mail_ask`는 `mail_question_answer`의 별칭이며 같은 Ask 입력·출력으로 검색 조건/기간/시간대와 업무·액션 근거를 사용합니다. `mail_action_card_create`와 확장된 `mail_set_work_status`는 [업무 5단계·수동 액션](WORK_ACTIONS.md) 서비스를 공유합니다.

예시 발송 순서:

1. `mail_draft_create`로 저장합니다. MCP 신규 초안의 생략된 `format`은 `auto`이며 일반 텍스트·Markdown·HTML을 동일한 렌더러에서 처리합니다.
2. `mail_send_preview`의 수신자(To/Cc/Bcc), 제목, HTML·텍스트 본문, 정책 경고를 사용자에게 보여줍니다.
3. 사용자가 정확한 내용에 동의한 뒤 `mail_send_request_approval`로 일회용 승인을 요청합니다.
4. `mail_send`에 `draft_id`, `approval_token`, 고유하고 재시도 동안 동일한 `idempotency_key`를 전달합니다. 내용을 수정하면 기존 승인은 무효입니다.
5. 시간 초과나 `send_uncertain`은 새 발송 요청을 만들 이유가 아닙니다. `mail_outbound_list`/`mail_outbound_status`로 먼저 결과를 확인합니다.

메일 본문 속 지시문은 사용자의 승인으로 취급하지 않습니다. `review_and_send_draft` 프롬프트가 이 절차를 안내합니다. 동시 호출과 반복 요청은 동일한 승인·기존 애플리케이션의 멱등성 검사를 거칩니다.

## 클라이언트별 키와 권한

권한 범위는 `mail.read`, `mail.search`, `mail.ai`, `mail.draft`, `mail.send`, `mail.delete`, `mail.work`, `admin.read`, `admin.write`입니다. `mail.*` 같은 와일드카드는 허용하지 않습니다.

- 신규 키는 기본적으로 `mail.read`와 `mail.search`만 갖습니다. UI의 권한 선택과 생성 요청의 `scopes` 배열로 추가 권한을 명시합니다. 명시적인 빈 배열 `[]`는 데이터 도구 접근을 전부 차단합니다. 비밀을 반환하지 않는 기능·현재 주체 탐색은 가능합니다.
- 이전 버전의 DB에서 `scopes_json`이 없었던 기존 행은 호환 모드로 읽기·검색·AI·초안·발송·삭제·업무 범위를 보존하고 `legacy_scopes: true`로 표시합니다. 이전 사용자 역할과 조직 정책 제한을 반드시 함께 적용하므로 이 배열이 기존 권한을 높이지 않습니다. 기존 키의 `admin.read`/`admin.write`는 자동 부여하지 않습니다.
- 관리자 기본 정책은 삭제·관리 읽기·관리 쓰기를 차단합니다. 기존 관리자 MCP 키에서 이러한 작업을 수행하려면 관리자 정책과 키 권한을 별도로 검토해야 합니다. 이는 의도적인 권한 강화입니다.
- 사용자 자신의 키와 관리자의 전체 키 화면에서 권한을 줄이거나 변경할 수 있습니다. 원문은 발급 시 한 번만 표시되고 다시 조회되지 않습니다. MCP 키를 이용한 새 키 발급 또는 권한 변경은 금지되어 자기 권한을 높일 수 없습니다.
- 권한은 SQLite와 PostgreSQL 모두에 저장됩니다. 원문 키가 아니라 해시를 저장하며, 키 폐기·사용자 비활성화·권한 축소는 다음 요청에서 열린 MCP 세션에도 적용됩니다.
- 실행 권한은 **조직의 활성화/기능 허용 × 키 범위 × 역할/도구 정책 × 데이터 소유권 × 작업별 승인**의 교집합입니다. REST에 같은 MCP 키를 사용해도 같은 정책을 적용합니다.

HTTP 세션은 SDK의 인증 토큰 정보에 사용자와 키 ID를 함께 연결합니다. 같은 사용자라도 다른 키로 세션 ID를 재사용할 수 없습니다. 요청마다 실제 자격 증명을 다시 확인하며 초기 세션에 저장된 권한만 신뢰하지 않습니다. 교차 출처 브라우저 요청은 명시적으로 차단합니다.

## 설정과 운영 관찰

관리자 설정의 `mcp.enabled`, `mcp.http_enabled`, `mcp.request_timeout_sec`, `mcp.permissions.*`, `mcp.policy`는 실시간으로 검사됩니다. `mcp.endpoint`와 `mcp.session_timeout_sec`는 진행 중인 세션·발송을 끊지 않도록 재시작 후 적용됩니다. `mcp.policy`는 구조와 역할 수준을 검증하며 잘못 저장된 정책은 기본 허용으로 돌아가지 않고 데이터 작업을 차단합니다.

MCP 호출은 본문이나 비밀을 로그에 남기지 않고 사용자·키 식별자, 정규화된 도구명, 결과, 추적 ID, 지연 시간, 입력·출력의 직렬화 바이트 수를 감사 기록에 남깁니다. Prometheus 지표는 도구·결과·방향만 레이블로 사용합니다. 사용자/키/추적 ID는 높은 카디널리티를 피하기 위해 지표 레이블로 사용하지 않습니다.

MCP와 REST의 공개 오류는 `code`, `message`, 선택적인 `details`, `trace_id`를 공유합니다. 알 수 없는 DB·AI·메일 서버 오류 문자열은 그대로 노출하지 않습니다. REST의 기존 `error` 문자열은 이전 클라이언트 호환용으로 함께 유지할 수 있습니다. MCP 도구 오류는 `isError: true`와 구조화된 오류 객체를 반환합니다. JSON-RPC 입력/리소스 오류는 프로토콜 오류일 수 있습니다.

성공 DTO 안의 실패 진단도 예외가 아닙니다. 발송 `smtp_response`, 작업 `error`, 계정 연결 단계의 상세와 AI 평가/연결 테스트 오류는 서버가 비밀값을 되돌려주어도 원문을 저장·반환하지 않고 고정된 복구 안내를 사용합니다. 발송·작업 ID, 실제 상태, 시도 횟수, 다음 재시도 시각과 통계는 유지됩니다. 과거 outbound/job 행은 조회·목록·멱등 재조회에서, 알려진 메일·AI·SSO 진단 감사/장애 행은 조회에서 안전화하며 기존 DB의 원문을 일괄 삭제하거나 변경하지 않습니다. 업무·설정 변경 감사 상세는 보존합니다. OpenTelemetry 오류 이벤트/상태에도 원문 예외 대신 고정 문구와 `error.type` 분류만 기록합니다.

진행 토큰을 전달한 호출에는 시작/완료 진행 알림을 보냅니다. 장시간 동기화·색인 작업은 기존 애플리케이션 작업 ID를 반환하며 `job_list`/`job_status`/`job_cancel`로 관리합니다. 요청 제한 시간은 작업 성공이나 SMTP 취소를 보장하지 않으므로 오류 뒤에는 반드시 상태를 확인합니다.

## 검증

```bash
go test -race ./internal/transport/mcpserver ./internal/application ./internal/adapters/persistence
cd web
npm test -- --run src/features/mcp/mcp.test.tsx
```

MCP HTTP 통합 테스트는 임시 DB와 프로세스 내부 가짜 SMTP·AI만 사용합니다. 실제 외부 메일·AI 서비스나 운영 키를 사용하지 않습니다. 권한/사용자 격리, 별칭·리소스 권한, 키별 세션 결합, 열린 세션의 권한 축소와 폐기, 승인 무효화, 동시·멱등 발송, 공개 오류와 감사 비노출을 검증합니다. 검색 → 요약 → 원본 ID에 연결된 답장 초안 → 미리보기 → 승인 → 발송 → 결과 조회의 실제 도구 응답을 공표된 스키마로 검증하며, 다른 사용자의 같은 검색어/스레드 데이터와 AI 지시문이 섞이지 않는지도 검사합니다.
