# Postra 이메일 통합 관리 플랫폼 관리자 가이드

본 가이드는 Postra 이메일 서비스를 운영, 관리, 모니터링하는 시스템 관리자(System Administrator) 및 DevOps/보안 엔지니어를 위한 종합 운영 매뉴얼입니다.

---

## 1. 시스템 아키텍처 및 운영 모드

Postra는 단일 바이너리 기반의 경량화 운영부터 기업급 멀티 파드(Multi-Pod) 고가용성 환경까지 유연하게 확장 가능하도록 포트-어댑터(Hexagonal Architecture) 구조로 설계되었습니다.

### 1.1 데이터베이스 및 운영 모드
1. **단일 노드 모드 (SQLite Mode)**
   - **스토리지**: SQLite 3 (WAL 모드 적용) + 온디스크 ObjectStore / SecretStore
   - **적용 환경**: 소규모 조직, 개인 배포, 개발 및 테스트 환경
2. **고가용성 멀티 노드 모드 (PostgreSQL + pgvector Mode)**
   - **스토리지**: PostgreSQL 15+ (`pgvector` 확장 모듈) + DB 암호화 SecretStore / ObjectStore
   - **적용 환경**: 엔터프라이즈 폐쇄망, K8s 다중 파드 환경, 무중단 HA 구성

---

## 2. 설치 및 환경 구성 (Deployment & Configuration)

### 2.1 주요 설정 환경변수 (Environment Variables)

| 환경변수명 | 기본값 | 설명 |
| --- | --- | --- |
| `POSTRA_HTTP_ADDR` | `127.0.0.1:8480` | REST API, Web UI, MCP 통합 바인딩 주소 |
| `POSTRA_DATA_DIR` | `./data` | SQLite, 암호화 객체, KEK 파일 저장 경로 |
| `POSTRA_STORAGE_DRIVER` | `sqlite` | 데이터베이스 드라이버 (`sqlite` 또는 `postgres`) |
| `POSTRA_DATABASE_URL` | - | PostgreSQL 접속 URI (`postgres://user:pass@host:5432/db`) |
| `POSTRA_KEK` | - | 마스터 암호화 키 (Base64 인코딩 32 Bytes). 주입 권장 |
| `POSTRA_ALLOW_INSECURE_MAIL` | `false` | 평문(Plain/Insecure) POP3/SMTP 허용 여부 |
| `POSTRA_BOOTSTRAP_ADMIN_PASSWORD` | - | 시스템 최초 기동 시 어드민 비밀번호 |
| `POSTRA_API_TOKEN` | - | 비로컬 HTTP 접근 시 API 인증 토큰 |

### 2.2 Docker 컨테이너 배포 예시
```bash
docker run -d \
  --name postra-server \
  -p 8480:8480 \
  -v /var/lib/postra:/data \
  -e POSTRA_HTTP_ADDR=0.0.0.0:8480 \
  -e POSTRA_KEK=YourBase64Encoded32ByteKeyHere= \
  -e POSTRA_ALLOW_INSECURE_MAIL=true \
  -e POSTRA_API_TOKEN=your-secure-api-token \
  postra:latest
```

---

## 3. 사용자 계정 및 권한 관리 (User Management)

### 3.1 어드민 계정 자동 생성 (EnsureUser 멱등성)
Postra는 시스템 최초 스타트업 시 어드민 계정(`admin@postra.local`)을 안전하게 초기화합니다.
- 계정이 존재하지 않는 경우 자동으로 신규 생성합니다.
- 기존 어드민 계정이 존재하는 경우, `SQLSTATE 23505 (users_pkey)` 충돌 없이 비밀번호 해시 및 역할을 멱등하게 업데이트합니다.

### 3.2 사용자 역할 (Roles)
- **Admin**: 사용자 관리, 수신 규칙 추가/삭제, 장애 대시보드 접근, 시스템 설정
- **User**: 메일 계정 연동, 수발신, 검색, 개인 수신 규칙 설정
- **ReadOnly**: 메일 읽기 및 검색 전용 권한

---

## 4. 암호화 SecretStore 및 KEK 로테이션 (Encryption Security)

Postra는 메일 서버 비밀번호, OIDC Client Secret, AI API Key 등 모든 민감 정보를 봉투 암호화(Envelope Encryption)하여 보관합니다.

### 4.1 KEK(Key Encryption Key) 및 DEK(Data Encryption Key) 작동 원리
1. 비밀값 저장 시 256-bit AES-GCM 무작위 DEK를 생성하여 데이터를 암호화합니다.
2. DEK는 32-Byte 마스터 KEK로 다시 래핑(Envelope)되어 저장됩니다.
3. KEK는 환경변수(`POSTRA_KEK`)로 주입하거나 영속 디렉터리의 `keyring.json`에 버전별로 안전하게 보관됩니다.

### 4.2 KEK 재발급 및 자가 치유 (Self-Healing)
- Pod 재기동 또는 볼륨 교체로 인해 KEK가 변경된 경우, `SecretStore.Rotate` 매커니즘이 구형 암호화 레코드를 감지하고 현재 활성화된 KEK로 자동 복구/재암호화 처리합니다.
- 관리자는 Web UI의 계정 수정 화면에서 비밀번호를 다시 저장하여 간편하게 로테이션시킬 수 있습니다.

---

## 5. 장애 모니터링 및 대시보드 (Incident Management)

Postra v0.10.0부터 제공되는 **장애 모니터링 대시보드**를 통해 시스템 이상을 실시간 감지할 수 있습니다.

### 5.1 장애 대시보드 접근
- **Web UI 경로**: `/ui/admin/incidents`
- **REST API 경로**: `/api/incidents`

### 5.2 모니터링 지표 및 장애 카테고리
1. **Sync Interruptions**: 동기화 도중 타임아웃 또는 서버 재기동으로 인한 중단 이력 (`RecoverStaleJobsExcept` 방어 내역)
2. **Authentication Failures**: POP3/SMTP 비밀번호 불일치 및 `secret_acquire` 복호화 실패
3. **Malware Blocked**: ClamAV/YARA에 의해 차단된 위험 첨부파일 이력
4. **Leader Election Flaps**: 멀티 파드 환경에서 리더 선출 상태 변경 이력

---

## 6. 백업 및 장애 복구 (Backup & Disaster Recovery)

### 6.1 백업 대상 데이터
1. **데이터베이스**: SQLite 파일(`postra.db`) 또는 PostgreSQL 백업 덤프
2. **SecretStore & KEK**: `secrets.enc.json` 및 `keyring.json` (또는 `POSTRA_KEK` 환경변수)
3. **오브젝트 스토어**: 메일 원문 및 첨부파일 디렉터리 (`/data/objects/`)

### 6.2 권장 백업 명령어
```bash
# SQLite DB 및 데이터 백업
tar -czvf postra-backup-$(date +%Y%m%d).tar.gz /var/lib/postra/data
```

---
*Postra 이메일 플랫폼 관리자 가이드 v0.10.0*
