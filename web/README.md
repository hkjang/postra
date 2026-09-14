# Postra React 워크스페이스

`/app/`은 React·TypeScript·Vite로 만든 브라우저 워크스페이스입니다. 기존 `/api`와 HttpOnly 로그인 세션을 사용하며, 기존 `/ui` 화면도 유지합니다. 운영 서버에서 Node, npm, 외부 CDN을 실행하거나 호출하지 않습니다.

## 개발

Node 22.12 이상과 프로젝트의 Go 도구 체인이 필요합니다. 저장소 루트에서 다음 순서로 실행합니다.

```bash
cd web
npm ci
npm run dev
```

개발 서버의 `/api`·`/ui` 요청은 `http://127.0.0.1:8480`으로 전달합니다. 별도 터미널에서 설정된 테스트용 Postra 서버를 실행하세요. 실제 메일 발송은 승인 후 수행되므로 개발에는 테스트 계정을 사용하세요.

## 빌드와 포함된 산출물

저장소 루트에서 `make build`를 실행하면 `npm ci`로 잠금 파일의 의존성을 설치하고 브라우저를 빌드한 뒤 Go 실행 파일에 포함합니다. 프런트엔드만 갱신하려면 `make frontend`를 사용합니다.

```bash
make frontend
make frontend-test
git status --short -- internal/transport/spa/assets
```

`npm run build`는 타입 검사, Vite 빌드, 제3자 라이선스 고지를 생성합니다. 결과는 `internal/transport/spa/assets`에 저장되며 Go `embed`로 포함됩니다. 프런트엔드 소스 또는 `package-lock.json`이 바뀌면 산출물도 함께 커밋하세요. CI와 릴리즈는 새로 빌드한 파일과 커밋된 파일이 다르면 실패합니다. `make frontend-check`로 같은 검사를 실행할 수 있습니다.

브라우저 의존성은 `web/package-lock.json`에 고정되어 있습니다. `npm install`로 버전을 변경했다면 잠금 파일 변경을 검토하고 테스트·빌드를 다시 수행하세요. 브라우저용 폰트도 패키지에서 번들링하므로 외부 폰트 서비스에 접속하지 않습니다.

## 오프라인 Go 빌드

프런트엔드 산출물을 커밋하므로 Node 없이 다음 명령으로 빌드할 수 있습니다.

```bash
make build-offline VERSION=v0.19.0
```

필요한 Go 버전과 Go 모듈 캐시는 미리 준비되어 있어야 합니다. 이 대상은 `GOPROXY=off`, `GOTOOLCHAIN=local`을 사용하여 네트워크 다운로드를 하지 않습니다. React 소스를 오프라인에서 수정하려면 Node와 해당 잠금 파일의 npm 캐시도 별도로 준비해야 합니다.

Docker 빌드는 Node 빌더에서 브라우저를 만든 뒤 Go 빌더로 복사합니다. 최종 distroless 이미지에는 Go 실행 파일만 추가되며 Node나 `node_modules`가 포함되지 않습니다. 폐쇄망 배포에는 릴리즈의 사전 빌드 이미지 tarball을 옮겨 사용하세요.

## 검사

```bash
cd web
npm run typecheck
npm test
npm audit --omit=dev --audit-level=high
```

`npm test`는 React 기능 테스트입니다. 실제 브라우저 회귀 검사는 Go HTTP 테스트 서버와 Playwright로 로그인·사용자별 개인정보 격리·CSRF·메일 및 관리 흐름을 검사합니다. 임시 데이터와 프로세스 내부의 가짜 SMTP·수신·AI 어댑터만 사용하므로 실제 메일이나 외부 AI 제공자에게 요청하지 않습니다.

```bash
cd web
npm ci
npx playwright install chromium
npm run build
npm run test:e2e
```

Linux에서 Chromium 시스템 라이브러리가 부족하면 `npx playwright install --with-deps chromium`을 사용하세요. Playwright의 Chromium 대신 설치된 Chrome을 쓰려면 `POSTRA_CHROME=/usr/bin/google-chrome npm run test:e2e`로 실행할 수 있습니다.

`npm run test:e2e`는 저장소의 `TestSPABrowser` Go 테스트를 활성화하며, 별도 서버나 운영 계정은 필요하지 않습니다. 스크린샷을 남기려면 미리 생성한 디렉터리를 `POSTRA_SPA_SCREENSHOT_DIR`로 지정하세요. CI는 Chromium을 설치한 뒤 같은 검사를 실행하고, 실패 시 해당 디렉터리를 `spa-browser-failure-screenshots` 아티팩트로 7일간 보관합니다.

## 릴리즈와 제3자 고지

릴리즈 워크플로는 프런트엔드를 먼저 빌드하고 커밋된 산출물을 검증한 뒤 바이너리·오프라인 이미지·체크섬을 생성합니다. Go SBOM에 더해 `postra-VERSION-frontend-sbom.cdx.json`을 첨부합니다. 태그에 대응하는 `docs/releases/vVERSION.md`가 있으면 그 내용을 릴리즈 노트로 사용하며, 없으면 GitHub 변경 내역을 자동 생성합니다.

`/app/THIRD_PARTY_NOTICES.txt`에는 잠금 파일에 포함된 프로덕션 의존성 및 폰트의 라이선스·저작권 고지를 포함합니다. 고지는 설치된 패키지에서 생성되며 타임스탬프나 네트워크 요청 없이 재현됩니다. npm 배포 파일에서 누락된 고지는 `web/licenses`에 보관합니다.
