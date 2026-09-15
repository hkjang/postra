# 업무 5단계와 액션 센터

메일 업무 상태는 `new`(새로 접수), `needs_action`(조치 필요), `in_progress`(진행 중), `waiting`(대기), `done`(완료)입니다. 기존 `open`, `pending`, `resolved` 입력과 저장 데이터는 각각 새로 접수, 진행 중, 완료의 별칭으로 계속 지원합니다. 기존 DB를 일괄 덮어쓰지 않습니다. SQLite/PostgreSQL 상태 필터는 이전 값과 새 값을 함께 찾으므로 `/ui`의 이전 필터도 같은 범위를 조회합니다.

React의 내 업무 기본 탭은 저장된 처리 업무의 5단계 보드이며, 기존 중요도·다시 확인·첨부 신호 분류는 **메일 신호** 탭에 유지합니다. 메일의 처리 관리에서 상태나 담당자를 지정하면 업무 보드에 등록됩니다. 담당자·SLA·내부 메모는 내 소유 메일에만 저장되며 메일로 발송되지 않습니다. 관리자도 다른 사용자의 메일 업무를 읽거나 변경할 수 없습니다. 보드는 최근 등록 업무 최대 200건이며 전체 보존 메일에 대한 완전한 집계가 아닙니다.

액션 센터는 오늘, 기한 초과, 예정, 대기·기한 확인, 완료·제외로 분류합니다. 유효한 ISO 날짜만 브라우저 현지 날짜와 비교하며 자연어로 추출된 AI 날짜를 임의 추측하지 않습니다. 날짜가 없거나 해석할 수 없으면 대기·기한 확인에 표시합니다. `done`과 `rejected`는 완료·제외, `exported`는 내보낸 데이터일 뿐 업무 완료가 아니므로 기한 분류를 계속 적용합니다.

`POST /api/action-cards`와 MCP `mail_action_card_create`는 동일한 수동 생성 서비스를 사용합니다. 원본 `message_id`와 `title`은 필수이며 `type`, `detail`, `due`, `assignee`를 선택적으로 입력합니다. 직접 입력한 기한은 `YYYY-MM-DD` 또는 시간대가 포함된 RFC3339 형식입니다. 원본 메일 소유권을 확인한 후 `pending` 상태로 생성합니다. AI를 호출하지 않으며 외부 업무·일정 시스템에 등록하지 않습니다. MCP는 `mail.work` 권한과 조직/역할 정책이 필요하고 REST 브라우저 변경은 CSRF 보호를 따릅니다.

검증:

```bash
go test -race ./internal/application ./internal/transport/httpapi -run 'Test(WorkFive|ManualAction)' -count=1
cd web
npm test -- --run src/features/work/work.test.tsx src/features/actions/actions.test.tsx
```
