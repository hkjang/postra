# 재시작 없는 동기화·AI 작업 설정

자동 동기화, AI 분류, 일일 요약, 자동 임베딩 worker는 시작 시 비활성화되어도 종료되지 않습니다. 리더 노드가 5초마다 설정을 확인하고 활성화·비활성화·주기 변경을 적용합니다. 이미 실행 중인 작업은 중단하지 않으며 다음 실행은 변경된 값을 사용합니다. 리더가 아닌 노드는 작업을 새로 시작하지 않습니다.

- `sync.auto_sync_minutes=0`은 조직 전체의 주기적 자동 동기화를 중지합니다. 수동 동기화와 별도 IMAP IDLE 동작은 이 주기 설정과 구별됩니다.
- 자동 동기화가 켜져 있으면 `account.auto_sync=false` 계정은 제외합니다. `account.sync_minutes=0`은 조직 기본 주기를 상속하며 양수는 계정 주기입니다. 조직의 `sync.min_sync_minutes`보다 짧게 실행할 수 없습니다.
- `sync.max_concurrent_syncs` 변경은 새 동기화 작업의 입장 제한에 적용됩니다. 낮추더라도 진행 중 작업을 끊지 않으며 사용 중 슬롯이 새 한도 아래로 내려갈 때까지 다음 작업이 대기합니다. 높이면 대기 중 작업이 최대 5초 안에 추가 슬롯을 감지합니다. 작업 취소는 슬롯 대기 중에도 가능합니다.
- `sync.triage_mode=off`는 자동 AI 분류를 중지합니다. `important_only`는 중요 표시된 메일, `rules`는 활성화된 사용자 규칙의 조건에 실제 일치하는 메일, `all`은 최근 미분류 메일을 대상으로 합니다. `rules` 분류 대상 확인은 규칙 동작을 실행하지 않습니다. 기존 수신 시 규칙 적용 단계는 그대로 유지됩니다.
- 새 분류 모드가 아직 저장되지 않은 경우에만 기존 `sync.auto_triage=true`를 `all`로 호환합니다. 명시적으로 저장한 `off`가 이전 활성화 값을 우선합니다. 분류는 최근 72시간 수신 메일 중 사용자별 최대 20건의 AI 호출로 제한됩니다. 비일치 메일이 앞쪽 페이지를 채워도 다음 페이지의 적합한 메일을 찾습니다.
- `sync.auto_triage_minutes`, `sync.auto_embed_minutes`, `sync.daily_digest_enabled`, `sync.daily_digest_hour`도 실행 시 확인합니다. 임베딩 간격 0은 비활성화입니다. 요약은 서버 시간대의 지정 시각 이후 기존 10분 확인 주기와 사용자별 일일 완료 기록을 따릅니다.

주기 변경·재활성화·슬롯 증감·계정별 설정·규칙/중요 필터는 시간을 분 단위로 기다리지 않는 결정적 테스트로 검증합니다.

```bash
go test -race ./internal/application -run 'Test(LiveWorker|DynamicSync|AccountSync|Scheduler|TriageModes)' -count=1
```
