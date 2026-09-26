# Postra 오프라인 배포 산출물

Docker 데몬 없이 빌드한, 오프라인망에서 바로 사용 가능한 산출물입니다.

> **v0.23.7 — 응답을 끝내지 않는 IMAP 서버가 메모리를 늘리던 문제 수정**: 줄 하나하나는 짧지만 태그 완료 응답을 끝내 보내지 않는 서버가 수집 작업을 영원히 붙잡아 두고 버퍼를 키우던 문제를 고친 패치 릴리즈입니다. 한 응답의 프로토콜 텍스트는 8 MiB, 리터럴 선언은 64개를 넘으면 세션을 폐기합니다. 리터럴 **바이트**는 세지 않으므로 상한을 끈 계정(`sync.max_message_bytes <= 0`)의 대용량 본문은 영향받지 않습니다. 새 설정 키·DB 마이그레이션·필수 환경변수는 없습니다. [릴리즈 내용](../docs/releases/v0.23.7.md)을 확인하세요. 프로토콜 줄 길이 상한은 [v0.23.6](../docs/releases/v0.23.6.md), 리터럴 길이 안전 처리는 [v0.23.5](../docs/releases/v0.23.5.md), 과대 리터럴 거부 후 스트림 재동기화는 [v0.23.4](../docs/releases/v0.23.4.md), MCP 도구 오류 스키마 수정은 [v0.23.3](../docs/releases/v0.23.3.md), MCP OAuth 세션 유지는 [v0.23.2](../docs/releases/v0.23.2.md), DCR 호환 OAuth 프록시는 [v0.23.0](../docs/releases/v0.23.0.md), Keycloak OAuth MCP 연결은 [v0.22.0](../docs/releases/v0.22.0.md)과 [설정 가이드](../docs/MCP_OAUTH.md)를 참고하세요. v0.19.x 이하에서 업그레이드할 경우 [v0.20.0의 백업·Keycloak 콜백·권한 변경 안내](../docs/releases/v0.20.0.md)도 먼저 확인하세요.

기존 SSO·메일 프로비저닝·비밀값 암호화·승인·멱등 발송은 유지합니다. 런타임 Node 서버나 외부 CDN은 필요 없으며 서체와 시간대 데이터도 실행 파일에 포함됩니다.

| 파일 | 설명 |
| --- | --- |
| `postra-0.23.7.tar.gz` | `docker load` 로 불러오는 컨테이너 이미지 (`postra:0.23.7`, linux/amd64) |
| `postra-0.23.7-linux-amd64` | 정적 링크 단일 실행 파일 (CGO 없음, 의존성 없음) |
| `postra-0.23.7-sbom.cdx.json` | CycloneDX 소프트웨어 자재명세서 |
| `postra-0.23.7-frontend-sbom.cdx.json` | 프런트엔드 의존성 CycloneDX 명세서 |
| `SHA256SUMS.txt` | 모든 릴리즈 파일의 SHA-256 체크섬 |

이미지는 순수 Go 정적 바이너리 + CA 인증서 + 최소 rootfs 로만 구성됩니다(scratch 기반).

## 1) Docker 로 실행 (오프라인 호스트)

```bash
# 폐쇄망 호스트로 tar.gz 를 옮긴 뒤:
docker load -i postra-0.23.7.tar.gz     # gzip 자동 인식
docker image ls postra

# 오프라인망(평문 POP3/SMTP 허용) 실행 예시
docker run -d --name postra \
  -p 8480:8480 \
  -v postra-data:/data \
  -e POSTRA_HTTP_ADDR=0.0.0.0:8480 \
  -e POSTRA_ALLOW_INSECURE_MAIL=true \
  -e POSTRA_API_TOKEN=change-me \
  postra:0.23.7

# CLI 사용 (같은 컨테이너)
docker exec -it postra postra account list
docker exec -it postra postra secret set --type mail_password --label "내 메일"
```

REST API는 `/api/v1/`(기존 `/api/` 호환), 유일한 공식 Web UI는 `/app/`, 인증은 `/auth/*`, MCP Streamable HTTP는 `/mcp`이며 기본적으로 8480 포트를 공유합니다. `/ui`의 템플릿·로그인·관리·정적 파일은 제거했습니다. 격리망에서도 로컬 관리자 또는 SSO 인증을 유지하고 예시 token/password는 반드시 교체하세요.

## 2) 바이너리 단독 실행 (Docker 불필요)

```bash
chmod +x postra-0.23.7-linux-amd64
./postra-0.23.7-linux-amd64 init
POSTRA_BOOTSTRAP_ADMIN_PASSWORD='replace-with-a-long-secret' ./postra-0.23.7-linux-amd64 serve
```

## 데이터 / 비밀값

- 모든 상태는 `/data`(컨테이너) 또는 `$POSTRA_DATA_DIR`(바이너리)에 저장됩니다: SQLite DB, 원본 MIME 객체, Envelope 암호화된 로컬 Secret Store, KEK.
- 서버 운영 시 KEK 는 온디스크 파일 대신 `POSTRA_KEK`(base64 32바이트) 로 주입하세요. Vault/OpenBao 연동 시 별도 Secret Store 어댑터로 교체할 수 있습니다.

## 이미지 재생성

Docker 없이 이미지를 다시 만들려면:

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o postra ./cmd/postra
go run scripts/mkimage.go postra postra-image.tar postra:0.23.7
gzip -9 postra-image.tar
```
