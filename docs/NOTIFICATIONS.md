# 개인 알림과 실시간 상태

`GET /api/events`는 인증된 동일 출처 SSE 연결입니다. 웹은 HttpOnly 세션 쿠키를 사용하며 키를 URL이나 브라우저 저장소에 넣지 않습니다. `GET /api/events?once=true`는 스트림을 사용할 수 없을 때의 JSON 조회이고 MCP `mail_events`도 같은 애플리케이션 스냅샷을 반환합니다.

스냅샷은 현재 사용자의 작업·발송·액션·보안 활동 ID, 범주, 상태, 시각만 포함합니다. 관리자는 자신의 이벤트만 받습니다. 메일 본문, 제목, 수신자, 액션 내용, 내부 메모, 작업 오류·진행 원문, SMTP 응답, 감사 상세와 자격 증명은 전송하지 않습니다. 조회는 최근 작업/발송/액션 각 50개, 감사 기록 100개로 제한됩니다. 알림은 전체 감사 보존소나 빠짐없는 이벤트 재생 스트림이 아닙니다.

조직 `notifications.enabled`와 개인의 `notifications.sync`, `send`, `ai`, `action`, `security`를 실제 조회 시 적용합니다. 관리자 잠금 정책은 개인 선택보다 우선합니다. `notifications.poll_seconds`는 메타데이터 갱신 간격이며 SSE 인증 재검증은 별도로 5초마다 수행합니다. 연결 중 세션이 만료·폐기되거나 키 범위가 줄어들면 다음 인증 확인에서 스트림을 닫습니다. 조직 활성화/주기 변경도 5초 확인에서 감지하고 범주 변경은 다음 스냅샷에서 적용합니다.

`preferences_revision`은 유효한 개인 설정의 해시만 전달합니다. 실제 설정값은 이벤트에 포함하지 않으며, 알림이 꺼져 있어도 이 해시는 유지됩니다. 웹은 변경을 감지하면 인증된 개인 설정 API를 다시 조회하여 테마·관리자 강제 정책을 반영합니다.

웹은 최초 스냅샷을 과거 알림으로 다시 띄우지 않으며 이후 실제 상태 변경만 표시합니다. 연결 장애 시 성공으로 표시하지 않고 설정 주기에 따른 JSON 조회로 전환합니다. 사용자 변경·로그아웃 시 스트림과 이전 토스트를 정리합니다. 알림을 꺼도 수동 작업 화면은 계속 사용할 수 있습니다. 이 기능은 브라우저 내 알림이며 운영체제 푸시 권한이나 외부 푸시 제공자를 요구하지 않습니다.

리버스 프록시는 `/api/events`의 응답 버퍼링을 끄고 스트림 연결을 허용해야 합니다. 서버는 `Cache-Control: no-store, no-transform`, `X-Accel-Buffering: no`와 주기적 keepalive를 보냅니다. HTTP 계측 래퍼는 `ResponseController`가 실제 writer까지 도달하도록 `Unwrap`을 지원합니다.

```bash
go test -race ./internal/application ./internal/transport/httpapi -run 'Test(Notification|Events)' -count=1
cd web
npm test -- --run src/features/notifications/notifications.test.tsx
```

## 사내 SMTP 릴레이 메일 알림

브라우저 알림과 별도로, 사람이 실제로 기다리는 네 가지 일(발송 실패로 멈춤, 담당 배정, 수집 인증 실패, 새 심각 장애)은 관리자가 `mail.enabled`를 켜면 사내 SMTP 릴레이로도 보냅니다. 기본은 꺼짐이며, 개인 알림 범주(`notifications.send`·`action`·`sync`·`security`)를 끈 사용자에게는 보내지 않습니다. 설정 표와 시험 발송은 [관리자 가이드 6.5](ADMIN_GUIDE.md#65-사내-smtp-릴레이-알림-메일-mail-standard)를 보세요.
