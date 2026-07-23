<p align="center">
  <img src="assets/logo.png" width="120" alt="Postra Logo"/>
</p>

# Postra — 개인 메일 AI · MCP 플랫폼


Go로 작성한 개인/사내 구축형 메일 서비스입니다. 사용자의 **POP3/IMAP/SMTP** 계정을 연결해 메일을 안전하게 **수집·검색·분석·작성·발송**하며, 모든 업무 기능을 **REST API / CLI / MCP / Web UI** 로 제공합니다.

## 설계 핵심

- **단일 비즈니스 코어** — REST·MCP·CLI 세 전송 계층이 모두 같은 `internal/application` 유스케이스를 호출합니다. 전송 계층에는 비즈니스 로직이 없습니다.
- **비밀값 비노출** — 메일 비밀번호·API Key는 MCP 도구 인수나 LLM 컨텍스트로 절대 전달되지 않습니다. Envelope 암호화(KEK→DEK, AES-256-GCM)로 저장하고, `SecretHandle`은 직렬화·문자열화가 불가능하며 어댑터 밖으로 나가지 않습니다.
- **저장 콘텐츠 암호화(at-rest)** — `encrypt_at_rest`(기본 켜짐)일 때 원본 MIME·첨부(객체 저장소)와 파싱 본문(DB body 컬럼)을 동일한 KEK로 암호화합니다. 검색·정렬에 필요한 메타데이터(제목·주소·날짜)는 조회 가능하도록 평문 유지됩니다. 단, 전문 검색을 위해 FTS 색인은 본문 평문을 담으므로, 색인까지 완전 암호화가 필요하면 FTS를 끄고 사용하세요.
- **사용자 승인 발송** — AI는 초안만 만들고, 실제 발송은 페이로드 해시에 묶인 일회성 승인 토큰을 요구합니다. 초안이 바뀌면 기존 토큰은 즉시 무효화됩니다.
- **메일 불신 원칙** — 메일 본문은 시스템 프롬프트와 명확히 구분된 비신뢰 데이터 블록에만 들어가며, 본문 내 지시는 도구 호출·발송으로 이어지지 않습니다.
- **오프라인망 지원** — TLS/인증 없이도 POP3·SMTP를 사용할 수 있습니다(격리망 전용, 정책 플래그로 명시적 허용 + 감사 기록).

## 빠른 시작

```bash
go build -o postra ./cmd/postra
./postra init                          # ~/.postra/config.json 생성

# 1) 비밀번호를 안전한 경로(TTY, 에코 없음)로 등록 → secret_ref 획득
./postra secret set --type mail_password --label "내 메일"

# 2) 계정 등록 (secret_ref 사용, 평문 비밀번호는 인수로 받지 않음)
./postra account add --name "내 메일" --email me@example.com \
  --pop3-host pop.example.com --pop3-security tls --pop3-user me \
  --pop3-secret-ref sec_xxx \
  --smtp-host smtp.example.com --smtp-security tls --smtp-user me --smtp-secret-ref sec_xxx

./postra account test --id acc_xxx     # DNS→TLS→AUTH→UIDL 단계별 진단
./postra sync --account acc_xxx --wait # POP3 수집
./postra search --q "invoice"

./postra serve                         # REST·Web UI + MCP(:8480/mcp)
./postra mcp                           # 로컬 MCP 클라이언트용 stdio 서버
```

처음 `/ui`에 접속하면 로컬 관리자 계정을 생성합니다. 서버를 원격에서 최초 기동할 때는
`POSTRA_BOOTSTRAP_ADMIN`과 `POSTRA_BOOTSTRAP_ADMIN_PASSWORD`로 관리자를 미리 생성하세요.

## 로그인·사용자 관리·Keycloak SSO

인증은 기본 활성화됩니다. 로컬 계정은 Argon2id로 해시하며, 세션 원문은 저장하지 않고
`HttpOnly`·`SameSite=Lax` 쿠키와 요청 Origin 검증을 사용합니다. 관리자는
`/ui/admin/users`에서 로컬 사용자를 생성하고 역할(`admin`/`user`), 활성 상태, 비밀번호를
관리할 수 있습니다. 마지막 활성 관리자는 비활성화하거나 강등할 수 없습니다. 사용자별
메일 계정·메시지·초안·검색·잡·감사 데이터는 애플리케이션과 저장소 양쪽에서 격리됩니다.

`/ui/admin/settings`에서는 세션, Keycloak OIDC, 메일 동기화, AI, 발송, 첨부 및 저장 보안
정책을 관리합니다. OIDC client secret은 설정 DB가 아니라 암호화 SecretStore에 저장됩니다.
환경변수는 최초 부트스트랩과 비밀 주입에 계속 사용할 수 있습니다.

Keycloak 클라이언트 설정:

1. Keycloak realm에 confidential OIDC client를 만들고 Standard Flow를 활성화합니다.
2. Valid Redirect URI를 `https://postra.example/ui/auth/oidc/callback`처럼 정확히 등록합니다.
3. `groups` claim에 그룹 경로를 포함하는 Group Membership mapper를 추가합니다.
4. Postra 관리 화면에서 issuer(`https://keycloak.example/realms/<realm>`), client ID,
   client secret, redirect URL, 관리자 그룹(기본 `postra-admins`)을 저장합니다.

Postra는 OIDC Discovery, Authorization Code Flow, S256 PKCE, state/nonce 검증, 서명·issuer·
audience 검증을 수행합니다. 최초 SSO 로그인 자동 생성 여부와 관리자 그룹 매핑도 관리
화면에서 제어합니다. REST와 원격 MCP는 같은 Keycloak access token을
`Authorization: Bearer <token>`으로 받을 수 있습니다.

```bash
POSTRA_BOOTSTRAP_ADMIN=admin \
POSTRA_BOOTSTRAP_ADMIN_PASSWORD='replace-with-a-long-secret' \
POSTRA_OIDC_ISSUER='https://keycloak.example/realms/postra' \
POSTRA_OIDC_CLIENT_ID='postra' \
POSTRA_OIDC_CLIENT_SECRET='replace-me' \
POSTRA_OIDC_REDIRECT_URL='https://postra.example/ui/auth/oidc/callback' \
./postra serve
```

공식 참고: [Keycloak OIDC endpoints](https://www.keycloak.org/securing-apps/oidc-layers),
[Keycloak application security guide](https://www.keycloak.org/securing-apps/overview).

## 주기 동기화

`config.json` 의 `sync.auto_sync_minutes` 를 0보다 크게 두면 `serve` 실행 시 백그라운드 스케줄러가 활성 POP3 계정을 해당 주기로 자동 동기화합니다(계정별 최소 간격 겸용). 또한 기동 시 재기동으로 중단된 `running`/`queued` job을 `failed` 로 정리해 유령 작업을 남기지 않습니다. 0이면 수동 동기화만 사용합니다.

## 첨부 보안

수집 시 모든 첨부는 정책 스캐너를 거칩니다(`internal/adapters/malware`).

- **위험 확장자 차단**(MIME-012) — `attachments.block_extensions`(기본: exe/js/vbs/msi/jar 등)는 내용을 저장하지 않고 다운로드를 거부합니다.
- **격리 확장자**(MIME-012) — `attachments.quarantine_extensions`(docm/dll/iso 등)는 저장하되 플래그를 달고, 다운로드 시 명시적 확인(`?ack=true`)을 요구합니다.
- **압축폭탄 방지**(MIME-011) — zip/gzip은 추출 없이 항목 수·총 해제 크기·압축비를 검사해 한도 초과 시 차단합니다(`archive_max_entries` / `archive_max_total_bytes` / `archive_max_ratio`).
- **스캔 상태 관리**(MIME-015) — 각 첨부는 `clean`/`quarantined`/`blocked`/`suspect` 상태로 기록됩니다. ClamAV 등 실제 AV는 동일 포트(`domain.AttachmentScanner`)로 교체 가능합니다.

## 발송 한도·경고

`config.json` 의 `send.max_per_minute` / `send.max_per_hour` 로 계정별 발송 속도를 제한합니다(0=무제한, 기본 20/분·200/시간). 멱등 재실행은 한도에 계산되지 않습니다. `send.warn_recipients` 이상 수신자를 대상으로 하면 발송 미리보기에 경고가 표시됩니다.

## 저장 백엔드 · 의미 검색

`storage_driver` 로 백엔드를 선택합니다.

- `sqlite`(기본) — 개인/임베디드. FTS5 전문 검색, 본문 at-rest 암호화.
- `postgres` — 서버/다중 사용자. `postgres_dsn` 필요. tsvector 전문 검색 + **pgvector** 의미 검색. `CREATE EXTENSION vector` 가능한 인스턴스가 필요합니다.

**의미(임베딩) 검색**은 두 백엔드 모두에서 동작합니다. `mail_embeddings_build`(또는 `POST /api/embeddings/build`)로 저장된 메일의 임베딩을 생성한 뒤 `mail_semantic_search`(`POST /api/semantic-search`)로 유사도 순 검색합니다. SQLite에서는 코사인 유사도를 Go로 계산하고, Postgres에서는 pgvector `<=>` 로 가속합니다. 결과에는 유사도 점수와 선택 이유가 포함됩니다. 임베딩 모델은 `ai.embed_model`(OpenAI 호환 `/embeddings`)로 지정합니다.

Postgres 어댑터 통합 테스트는 `POSTRA_TEST_PG` DSN이 설정된 경우에만 실행됩니다(미설정 시 skip).

## 수집 프로토콜 (POP3 / IMAP)

인바운드 수집은 **POP3**(기본)와 **IMAP** 을 지원합니다. 계정 등록 시 `--inbound-protocol imap` 로 선택하며, 이후 수집·검색·분석·삭제 경로는 동일합니다(IMAP 세션이 동일 인바운드 포트를 구현). 포트 미지정 시 프로토콜·보안 모드에 따라 기본값이 정해집니다(POP3 995/110, IMAP 993/143). IMAP은 `UIDVALIDITY.UID` 를 중복 제거 체크포인트로 사용합니다.

```bash
./postra account add --name Work --email me@corp.local \
  --inbound-protocol imap --pop3-host imap.corp.local --pop3-security tls \
  --pop3-user me --pop3-secret-ref sec_xxx  --smtp-host smtp.corp.local
```

> 인바운드 서버 좌표는 프로토콜과 무관하게 `--pop3-*` 플래그로 지정합니다.

## 오프라인 / 격리망

TLS·인증이 없는 사내 메일 서버(POP3/IMAP/SMTP)는 다음으로 허용합니다(감사 로그에 기록됨):

```bash
POSTRA_ALLOW_INSECURE_MAIL=true ./postra serve
# 계정 등록 시 --pop3-security none --smtp-security none --smtp-auth none
```

## MCP

- **로컬(stdio)**: `postra mcp` — Claude Code 등 로컬 MCP 클라이언트에 연결
- **원격(Streamable HTTP)**: `postra serve` 의 `http://127.0.0.1:8480/mcp`. REST·Web UI와 같은 포트를 사용하며 비로컬 인터페이스 바인딩 시 `POSTRA_API_TOKEN` 을 요구합니다. 기존 배포 호환용 별도 리스너가 필요할 때만 `mcp_http_addr`/`POSTRA_MCP_HTTP_ADDR`를 설정하세요.

주요 **도구(Tools)**: `mail_account_*`, `secret_registration_begin`, `mail_sync_start`, `job_status`, `mail_search`, `mail_message_get`, `mail_thread_get`, `mail_summarize` / `mail_classify` / `mail_action_items_extract` / `mail_phishing_inspect` / `mail_question_answer`, `mail_draft_create`, `mail_draft_rewrite`, `mail_send_preview`, `mail_send_request_approval`, `mail_send`, `mail_local_delete`, `mail_server_delete_preview`, `mail_server_delete_request_approval`, `mail_server_delete`, `mail_audit_search`.

**리소스(Resources)**: `mail://accounts/{id}`, `mail://messages/{id}`, `mail://messages/{id}/raw`, `mail://threads/{id}`, `mail://drafts/{id}`, `mail://sync-jobs/{id}`, `policy://mail/current`, `schema://mail/tools`.

**프롬프트(Prompts)**: `summarize_mail`, `summarize_thread`, `draft_reply`, `extract_action_items`, `review_phishing_risk`, `rewrite_formal`, `rewrite_concise`, `prepare_daily_digest`.

## Web UI (§ mail-web)

`serve` 는 REST 바인드 주소의 `/ui` 에 서버 렌더링 웹 UI를 제공합니다(`config.json` 의 `web_ui_enabled=false` 로 비활성). 외부 의존·빌드 단계·CDN 없이 Go `html/template` + 임베드 자산으로 단일 바이너리에 포함되어 **오프라인망에서 그대로 동작**합니다.

- **온보딩·계정** (`/ui/accounts`) — 비밀번호 암호화 등록, POP3/IMAP·SMTP 설정, 단계별 연결 진단, 수동 동기화와 진행 상태
- **받은편지함·검색** (`/ui/`) — 최신 메일 목록, 계정 필터, 제목·본문·보낸이 검색, 커서 페이지 이동
- **메일 상세** (`/ui/messages/{id}`) — 헤더·본문, 안전한 첨부 다운로드, 요약·할 일·피싱 AI 분석, 답장·전체 답장·전달 초안
- **새 메일·초안** (`/ui/compose`, `/ui/drafts/{id}`) — 직접 또는 AI로 초안 작성, 수신자·제목·본문 편집, AI 문체 재작성
- **발송 승인** (`/ui/drafts/{id}/send`) — 미리보기·경고(외부 도메인/다수 수신자) → **승인 요청** → **발송 확정** 2단계. 초안이 바뀌면 토큰 무효화, 동일 버전 재전송은 멱등(이중 발송 없음)

UI는 로컬 계정 또는 Keycloak SSO 로그인(`/ui/login`)을 요구합니다. 기존 `APIToken`은 자동화·
복구용 REST/MCP bearer credential과 인증 비활성화 호환 모드에서만 사용합니다. 서버는 평문
HTTP로 서빙하므로 인터넷 노출 시 신뢰하는 리버스 프록시에서 TLS를 종단하고 정확한 OIDC
redirect URI를 사용하세요.

## 관측성 · 메트릭 (§18.1)

`serve` 는 REST 바인드 주소에 Prometheus 메트릭을 `GET /metrics` 로 노출합니다(스크레이핑용으로 **인증 불요**, `config.json` 의 `metrics_enabled=false` 로 비활성). 노출 범위는 `http_addr` 바인딩으로 제어하세요.

| 메트릭 | 타입 | 라벨 | 의미 |
|--------|------|------|------|
| `postra_pop3_sync_total` | counter | `status` | POP3 동기화 잡 종료 상태별 수 |
| `postra_pop3_messages_fetched_total` | counter | — | 신규 수집 메일 수 |
| `postra_ai_requests_total` | counter | `op`,`result` | AI 호출(generate/embed) 수 |
| `postra_ai_request_duration_seconds` | histogram | `op` | AI 호출 지연 |
| `postra_smtp_send_total` | counter | `result` | 발송 결과별(sent/deferred/uncertain/failed) |
| `postra_smtp_retry_total` | counter | — | Outbox 재시도 처리 수 |
| `postra_outbox_pending` | gauge | — | 재발송 대기 중인 발신 메일 |
| `postra_mcp_requests_total` | counter | `tool`,`result` | MCP 도구 호출 수 |
| `postra_http_requests_total` | counter | `route`,`method`,`code` | REST 요청(경로 패턴 라벨) |
| `postra_http_request_duration_seconds` | histogram | `route` | REST 요청 지연 |
| `postra_ui_actions_total` | counter | `action`,`result` | 계정 연결→동기화→AI→초안→승인·발송의 UI 제품 여정 |

Go 런타임·프로세스 표준 메트릭(`go_*`, `process_*`)과 배포 버전(`postra_build_info`)도 함께 노출됩니다. 라벨은 저카디널리티(계정·메시지 ID 미포함)로 유지해 장기 구동 시 시계열 폭증을 막습니다.

**헬스 프로브**(인증 불요): `GET /api/livez` 는 프로세스 생존(항상 200), `GET /api/readyz`(및 `/api/healthz`)는 저장소 도달 가능 여부를 확인해 실패 시 503을 반환합니다. K8s liveness/readiness 프로브에 각각 매핑하세요.

## 백업과 복구

Postra의 영속 데이터는 모두 `data_dir`(Docker는 `/data` 볼륨)에 있습니다. SQLite DB, 암호화된 원문·첨부, 암호화 SecretStore와 KEK가 함께 있어야 복구할 수 있습니다.

1. `postra serve`를 정상 종료해 진행 중인 동기화·발송 작업을 멈춥니다.
2. `data_dir` 전체를 권한과 디렉터리 구조를 보존하는 도구로 백업합니다. 일부 파일만 복사하거나 KEK를 제외하면 복구할 수 없습니다.
3. 복구할 때는 Postra를 정지한 상태에서 빈 `data_dir`에 백업 전체를 복원합니다.
4. 동일하거나 호환되는 Postra 버전으로 기동하고 `/api/readyz`, 계정 목록, 저장 메일과 첨부 열기를 확인합니다.
5. 별도 주기에 복구 훈련을 수행하고 백업 자체도 운영 환경과 분리해 암호화·접근 통제합니다.

PostgreSQL 모드는 DB 공급자의 일관된 스냅샷과 `data_dir` 객체·SecretStore·KEK를 같은 복구 시점으로 함께 보관해야 합니다. 실행 중인 SQLite 파일을 단순 복사하는 방식은 일관성을 보장하지 않으므로 사용하지 마세요.

## 구조 (§15)

```
cmd/postra            단일 바이너리 (api+mcp+worker+cli)
internal/domain       엔티티·포트 인터페이스 (SecretHandle, POP3/SMTP/AI/Approval)
internal/application  유스케이스 (accounts, sync, query, analysis, compose, send)
internal/adapters     pop3, smtp, mailparse, ai(OpenAI 호환), secretstore, persistence(SQLite), objectstore
internal/transport    httpapi(REST), mcpserver(공식 Go MCP SDK)
internal/platform     config, crypto(envelope)
```

MVP는 단일 바이너리이지만 내부 패키지는 `mail-api` / `mail-mcp` / `mail-worker` 경계로 분리되어 있어 향후 프로세스 분리가 가능합니다.

## 테스트

```bash
go test ./...            # 기능·보안 테스트
go test -race ./...      # 동시성 검사
```

수집 멱등성, UIDL 미지원 폴백, 승인 토큰 무효화, 발송 멱등성, CRLF 헤더 인젝션 차단, Prompt Injection 격리, 비밀값 비평문, 감사 추적, 비활성 계정 차단, send_uncertain 무재발송, 스레딩, 원본 MIME 보존, 초안 버전 작성자 구분을 커버합니다.

## CI · 공급망 보안 (§20.3)

`.github/workflows/ci.yml` 가 push/PR 마다 아래를 자동 실행합니다.

| 잡 | 내용 |
|----|------|
| **build · vet · test** | `go build` / `go vet` / `go test -race -cover`, `go mod tidy` drift 검사 |
| **govulncheck** | Go 취약점 DB 대조 (호출 그래프 기반) |
| **gosec** | 정적 보안 스캐너. medium+ 심각도는 차단, LOW(G104)는 리포트만. 의도된 항목은 `#nosec <규칙> -- <근거>` 로 명시 |
| **SBOM** | CycloneDX(`cyclonedx-gomod`) 소프트웨어 자재명세서 아티팩트 |
| **docker** | 배포 이미지 빌드 검증 |

빌드 툴체인은 `go.mod` 의 `toolchain` / Dockerfile / 워크플로 `GO_VERSION` 에서 동일 패치 버전으로 고정하며, 셋을 함께 올립니다(표준 라이브러리 보안 픽스 반영). 보안 스캐너(`govulncheck`·`gosec`·`cyclonedx-gomod`)도 고정 버전으로 설치해 재현성을 확보합니다.

**릴리즈 자동화**: `release.yml` 이 `v*` 태그 push 시 버전 각인 정적 바이너리·오프라인 이미지 tarball·SBOM·체크섬을 빌드해 GitHub Release로 발행합니다. 버전은 `-ldflags -X …/build.Version` 로 주입되어 `postra version`·MCP·`postra_build_info` 메트릭에 일관 반영됩니다.

로컬에서도 동일 검사 실행:

```bash
go run golang.org/x/vuln/cmd/govulncheck@latest ./...
go run github.com/securego/gosec/v2/cmd/gosec@latest -severity medium -exclude-dir=scripts ./...
```

## 확장 (post-MVP)

Vault/OpenBao SecretStore, S3 ObjectStore, PostgreSQL+pgvector, OAuth2 인증, 규칙 자동화. 모든 외부 의존은 포트 인터페이스 뒤에 있어 어댑터 교체만으로 확장됩니다.
