# Web/MCP 통합 및 설정 콘솔 구현 체크리스트

기준: v0.19.1 (`099fa58`), 2026-09-15. 기존 데이터·승인·소유권·폐쇄망 배포를 보존한다. 완료 확인 전에는 해당 항목을 완료로 표시하지 않는다.

## P0 — 공식 Web UI와 공통 계약

- [x] `/` → `/app/`, React login/setup/error, `/auth/*`, v0.19.1 OIDC 오류·일회용 쿠키·깊은 링크·CSRF·자동로그인 검증
- [x] Legacy route/feature parity 표: inbox, message/thread/search/semantic, compose/draft/sent/accounts/work/actions/rules/AI/admin/users/security/audit/auth/setup/error
- [x] 누락 기능 이전 후 `internal/transport/webui` 제거, 한 버전 전환용 URL shim만 허용, Keycloak callback 변경 가이드
- [x] 서버 전체 초안 목록·읽음 상태·발송 첨부 계약 및 사용자별 소유권
- [x] Text/Markdown/HTML/auto 공통 렌더러, 인라인 템플릿, HTML/plain 대안, MIME 및 보안 golden fixtures
- [x] REST/MCP 공통 오류·trace 계약, MCP discovery/schema/scopes, 승인·버전/hash·DLP·idempotency 보존
- [x] MCP 찾기→요약→초안→미리보기→승인→발송→상태 E2E 및 부정·권한·동시성 계약 테스트

## 설정 관리 — 운영 가능한 값만 제공

- [x] 단일 설정 정의/환경변수 바인딩, 기본값→파일/환경 초기값→관리자→사용자/계정, 강제 정책 우선
- [x] 카테고리/환경변수 검색·값 출처·변경 미리보기·재시작 표시·등록 여부만 보이는 비밀값·키별 감사
- [x] 관리자 일반/인증/메일/AI/검색/MCP/발송/첨부/동기화/보안/알림/저장소/시스템·사용자·감사
- [x] AI endpoint/key/models/context/temperature/token/timeout/task routing/비활성모델/외부허용/PII 및 연결 테스트
- [x] 실시간 정책 반영: AI, 수집 간격/동시성, 발송 제한, MCP 권한/timeout, 알림/UI
- [x] bootstrap-only 데이터 경로/KEK/DB 변경 등 안전하게 hot-swap할 수 없는 값은 배포 전용으로 명시
- [x] 사용자 theme/density/reader/snippet/AI panel/default account/search/locale/date/notification/compose style
- [x] 계정별 수집·서명·template·AI style, 복수 서명과 작성 중 선택/자동 적용

## P1 — 업무 워크스페이스

- [x] 통합 AI Insight와 Ask Postra 근거/검색/업무/액션 맥락
- [x] Work 단계·SLA, Today/Overdue/Upcoming/Waiting/Completed 액션, 자동 분류 정책
- [x] 키보드·명령 팔레트·Copy MCP Context·접근성
- [x] Composer 선택 영역 AI 재작성·attachment drag/paste·보안 검사·발송 위험 미리보기
- [x] MCP 운영 UI·최근 호출·scope 관리·audit/metrics
- [x] SSE 알림과 Web/MCP 공통 Job 상태/진행률·취소
- [x] 수신/발송 HTML 보안 분리 및 외부이미지/정책 우선순위

## 추가 기능과 품질

- [x] 기존 MCP resources/prompts 확장 및 CID/서명 이미지 검증
- [x] API v1 alias/정식 계약 문서·schema drift 검증
- [x] SQLite/PostgreSQL 동등 저장 계약, unit/integration/MCP/E2E/OIDC/golden/SMTP/security/a11y
- [x] 프런트 번들·Go embed·오프라인 Docker·SBOM·checksum
- [ ] 마이그레이션·릴리즈 노트, 커밋/푸시/CI/릴리즈와 배포 산출물 확인

로컬 최종 검증: Go 1.26.6 전체 race/vet, React 32개 파일·138개 테스트, 실제 브라우저 3개 여정, MCP 실제 HTTP 도구 흐름, PostgreSQL/pgvector 재시작·CAS, 실제 TCP SMTP와 HTML/MIME golden, gosec medium+ 및 호출 경로 govulncheck 통과. 생산 프런트 의존성 audit도 통과했다. 릴리즈 발행은 소스 커밋 시점에 미리 완료 처리하지 않으며 GitHub CI/Release와 게시 산출물 검증 결과를 최종 기록으로 삼는다.

고급 자동화·외부 서비스 자동 실행·사용자 문체 학습 등은 요구된 권한 경계와 공통 application service 없이 UI 모형으로 대체하지 않는다. 전환 도중 발견한 누락과 실제 지원 범위는 이 체크리스트와 parity 문서에서 추적한다.
