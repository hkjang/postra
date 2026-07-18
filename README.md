# Postra — 개인 메일 AI · MCP 플랫폼

Go로 작성한 개인/사내 구축형 메일 서비스입니다. 사용자의 **POP3/SMTP** 계정을 연결해 메일을 안전하게 **수집·검색·분석·작성·발송**하며, 모든 업무 기능을 **REST API / CLI / MCP** 세 인터페이스로 동일하게 제공합니다.

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

./postra serve                         # REST(:8480) + MCP Streamable HTTP(:8481)
./postra mcp                           # 로컬 MCP 클라이언트용 stdio 서버
```

## 주기 동기화

`config.json` 의 `sync.auto_sync_minutes` 를 0보다 크게 두면 `serve` 실행 시 백그라운드 스케줄러가 활성 POP3 계정을 해당 주기로 자동 동기화합니다(계정별 최소 간격 겸용). 또한 기동 시 재기동으로 중단된 `running`/`queued` job을 `failed` 로 정리해 유령 작업을 남기지 않습니다. 0이면 수동 동기화만 사용합니다.

## 오프라인 / 격리망

TLS·인증이 없는 사내 메일 서버는 다음으로 허용합니다(감사 로그에 기록됨):

```bash
POSTRA_ALLOW_INSECURE_MAIL=true ./postra serve
# 계정 등록 시 --pop3-security none --smtp-security none --smtp-auth none
```

## MCP

- **로컬(stdio)**: `postra mcp` — Claude Code 등 로컬 MCP 클라이언트에 연결
- **원격(Streamable HTTP)**: `postra serve` 의 `:8481`. 비로컬 인터페이스 바인딩 시 `POSTRA_API_TOKEN` 을 요구합니다.

주요 **도구(Tools)**: `mail_account_*`, `secret_registration_begin`, `mail_sync_start`, `job_status`, `mail_search`, `mail_message_get`, `mail_thread_get`, `mail_summarize` / `mail_classify` / `mail_action_items_extract` / `mail_phishing_inspect` / `mail_question_answer`, `mail_draft_create`, `mail_draft_rewrite`, `mail_send_preview`, `mail_send_request_approval`, `mail_send`, `mail_local_delete`, `mail_server_delete_preview`, `mail_server_delete_request_approval`, `mail_server_delete`, `mail_audit_search`.

**리소스(Resources)**: `mail://accounts/{id}`, `mail://messages/{id}`, `mail://messages/{id}/raw`, `mail://threads/{id}`, `mail://drafts/{id}`, `mail://sync-jobs/{id}`, `policy://mail/current`, `schema://mail/tools`.

**프롬프트(Prompts)**: `summarize_mail`, `summarize_thread`, `draft_reply`, `extract_action_items`, `review_phishing_risk`, `rewrite_formal`, `rewrite_concise`, `prepare_daily_digest`.

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

## 확장 (post-MVP)

Vault/OpenBao SecretStore, S3 ObjectStore, PostgreSQL+pgvector, IMAP/OAuth2 어댑터, 규칙 자동화. 모든 외부 의존은 포트 인터페이스 뒤에 있어 어댑터 교체만으로 확장됩니다.
