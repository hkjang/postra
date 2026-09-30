# Postra 오프라인 배포 산출물

Docker 데몬 없이 빌드한, 오프라인망에서 바로 사용 가능한 산출물입니다.

> **v0.24.0 — 보낸 메일 동기화, 동기화 중단 수정, 조직 계정 로그인**: IMAP 보낸편지함과 Postra에서 보낸 메일을 보낸 메일로 저장하고, MCP `mail_sent_search` 로 보낸 메일만 조회합니다. 보낸 메일은 받은편지함·검색 기본 보기·규칙·AI 기능에 섞이지 않습니다. IMAP IDLE 감시가 패닉이나 짧은 리더 전환 뒤 다시 시작되지 않던 문제, 리더 lease 갱신이 멈추거나 만료 뒤에도 리더로 남던 문제를 고쳤습니다. 로그인 화면은 조직 계정(SSO)이 주 동작이 되고, silent SSO 로그아웃이 새 탭에서도 유지되며, 미연결 Keycloak 사용자는 첫 MCP 요청에서 웹 SSO와 같은 규칙으로 연결됩니다. **`messages.mailbox` 컬럼과 인덱스를 추가하는 DB 마이그레이션**이 기동 시 자동으로 실행됩니다. 보낸 메일이 저장된 뒤 v0.23.x 로 되돌리면 보낸 메일이 받은편지함에 섞이니 백업 DB로 복원하세요. [릴리즈 내용](../docs/releases/v0.24.0.md)을 확인하세요. Keycloak OAuth MCP 연결은 [설정 가이드](../docs/MCP_OAUTH.md)를 참고하세요. v0.19.x 이하에서 업그레이드할 경우 [v0.20.0의 백업·Keycloak 콜백·권한 변경 안내](../docs/releases/v0.20.0.md)도 먼저 확인하세요.

기존 SSO·메일 프로비저닝·비밀값 암호화·승인·멱등 발송은 유지합니다. 런타임 Node 서버나 외부 CDN은 필요 없으며 서체와 시간대 데이터도 실행 파일에 포함됩니다.

| 파일 | 설명 |
| --- | --- |
| `postra-0.24.0.tar.gz` | `docker load` 로 불러오는 컨테이너 이미지 (`postra:0.24.0`, linux/amd64) |
| `postra-0.24.0-linux-amd64` | 정적 링크 단일 실행 파일 (CGO 없음, 의존성 없음) |
| `postra-0.24.0-sbom.cdx.json` | CycloneDX 소프트웨어 자재명세서 |
| `postra-0.24.0-frontend-sbom.cdx.json` | 프런트엔드 의존성 CycloneDX 명세서 |
| `SHA256SUMS.txt` | 모든 릴리즈 파일의 SHA-256 체크섬 |

이미지는 순수 Go 정적 바이너리 + CA 인증서 + 최소 rootfs 로만 구성됩니다(scratch 기반).

## 1) Docker 로 실행 (오프라인 호스트)

```bash
# 폐쇄망 호스트로 tar.gz 를 옮긴 뒤:
docker load -i postra-0.24.0.tar.gz     # gzip 자동 인식
docker image ls postra

# 오프라인망(평문 POP3/SMTP 허용) 실행 예시
docker run -d --name postra \
  -p 8480:8480 \
  -v postra-data:/data \
  -e POSTRA_HTTP_ADDR=0.0.0.0:8480 \
  -e POSTRA_ALLOW_INSECURE_MAIL=true \
  -e POSTRA_API_TOKEN=change-me \
  postra:0.24.0

# CLI 사용 (같은 컨테이너)
docker exec -it postra postra account list
docker exec -it postra postra secret set --type mail_password --label "내 메일"
```

REST API는 `/api/v1/`(기존 `/api/` 호환), 유일한 공식 Web UI는 `/app/`, 인증은 `/auth/*`, MCP Streamable HTTP는 `/mcp`이며 기본적으로 8480 포트를 공유합니다. `/ui`의 템플릿·로그인·관리·정적 파일은 제거했습니다. 격리망에서도 로컬 관리자 또는 SSO 인증을 유지하고 예시 token/password는 반드시 교체하세요.

## 2) 바이너리 단독 실행 (Docker 불필요)

```bash
chmod +x postra-0.24.0-linux-amd64
./postra-0.24.0-linux-amd64 init
POSTRA_BOOTSTRAP_ADMIN_PASSWORD='replace-with-a-long-secret' ./postra-0.24.0-linux-amd64 serve
```

## 데이터 / 비밀값

- 모든 상태는 `/data`(컨테이너) 또는 `$POSTRA_DATA_DIR`(바이너리)에 저장됩니다: SQLite DB, 원본 MIME 객체, Envelope 암호화된 로컬 Secret Store, KEK.
- 서버 운영 시 KEK 는 온디스크 파일 대신 `POSTRA_KEK`(base64 32바이트) 로 주입하세요. Vault/OpenBao 연동 시 별도 Secret Store 어댑터로 교체할 수 있습니다.

## 이미지 재생성

Docker 없이 이미지를 다시 만들려면:

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o postra ./cmd/postra
go run scripts/mkimage.go postra postra-image.tar postra:0.24.0
gzip -9 postra-image.tar
```
