# Postra 오프라인 배포 산출물

Docker 데몬 없이 빌드한, 오프라인망에서 바로 사용 가능한 산출물입니다.

> **v0.4.0 새 기능**: Web UI만으로 계정 연결·진단·동기화·받은편지함·AI 분석·첨부 다운로드·초안 작성/편집·승인 발송까지 수행하는 완결된 메일 워크스페이스.

| 파일 | 설명 |
| --- | --- |
| `postra-0.4.0-linux-amd64-image.tar.gz` | `docker load` 로 불러오는 컨테이너 이미지 (`postra:0.4.0`, linux/amd64) |
| `postra-0.4.0-linux-amd64` | 정적 링크 단일 실행 파일 (CGO 없음, 의존성 없음) |
| `postra-0.4.0-sbom.cdx.json` | CycloneDX 소프트웨어 자재명세서 |
| `SHA256SUMS.txt` | 모든 릴리즈 파일의 SHA-256 체크섬 |

이미지는 순수 Go 정적 바이너리 + CA 인증서 + 최소 rootfs 로만 구성됩니다(scratch 기반).

## 1) Docker 로 실행 (오프라인 호스트)

```bash
# 폐쇄망 호스트로 tar.gz 를 옮긴 뒤:
docker load -i postra-0.4.0-linux-amd64-image.tar.gz     # gzip 자동 인식
docker image ls | grep postra

# 오프라인망(평문 POP3/SMTP 허용) 실행 예시
docker run -d --name postra \
  -p 8480:8480 -p 8481:8481 \
  -v postra-data:/data \
  -e POSTRA_HTTP_ADDR=0.0.0.0:8480 \
  -e POSTRA_MCP_HTTP_ADDR=0.0.0.0:8481 \
  -e POSTRA_ALLOW_INSECURE_MAIL=true \
  -e POSTRA_API_TOKEN=change-me \
  postra:0.4.0

# CLI 사용 (같은 컨테이너)
docker exec -it postra postra account list
docker exec -it postra postra secret set --type mail_password --label "내 메일"
```

비로컬 인터페이스(`0.0.0.0`)로 바인딩하므로 `POSTRA_API_TOKEN` 설정을 권장합니다. 완전 격리망이라면 생략 가능하나 기동 시 경고가 출력됩니다.

## 2) 바이너리 단독 실행 (Docker 불필요)

```bash
chmod +x postra-0.4.0-linux-amd64
./postra-0.4.0-linux-amd64 init
POSTRA_ALLOW_INSECURE_MAIL=true ./postra-0.4.0-linux-amd64 serve
```

## 데이터 / 비밀값

- 모든 상태는 `/data`(컨테이너) 또는 `$POSTRA_DATA_DIR`(바이너리)에 저장됩니다: SQLite DB, 원본 MIME 객체, Envelope 암호화된 로컬 Secret Store, KEK.
- 서버 운영 시 KEK 는 온디스크 파일 대신 `POSTRA_KEK`(base64 32바이트) 로 주입하세요. Vault/OpenBao 연동 시 별도 Secret Store 어댑터로 교체할 수 있습니다.

## 이미지 재생성

Docker 없이 이미지를 다시 만들려면:

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o postra ./cmd/postra
go run scripts/mkimage.go postra postra-image.tar postra:0.4.0
gzip -9 postra-image.tar
```
