# Legacy Web UI retirement parity

This inventory compares the v0.19.1 Go template router with the standalone React workspace and JSON transports. After the mailbox/auth/settings/SMTP browser gates passed and root approved retirement, all 46 tracked files under `internal/transport/webui` and the superseded legacy browser script were removed. Existing Git history can restore them if needed; no stored user/mail data was deleted. The bookmarks below are the only v0.20 compatibility shims.

## Authentication and bookmarks

| Legacy route | Replacement | State / invariant |
| --- | --- | --- |
| `GET/POST /ui/setup` | `/app/setup`, `GET/POST /auth/setup` | Implemented. One initial administrator; loopback/bootstrap deployment gate; strict Origin + JSON for setup write. |
| `GET/POST /ui/login` | `/app/login`, `POST /auth/login` | Implemented. Local password and legacy token mode; HttpOnly cookie, readable separate CSRF; no credentials in JSON/storage. |
| `POST /ui/logout` | `POST /auth/logout` | Implemented. Server session revoked; SPA clears identity-scoped cache; explicit logout suppresses silent SSO. Workspace navigation integration is part of root UI work. |
| `GET /ui/auth/oidc/start` | `GET /auth/oidc/start` | Implemented. prompt=none loop guard, signed state/nonce and safe deep return to `/app`. |
| `GET /ui/auth/oidc/callback` | 307 to `/auth/oidc/callback` preserving query only | Implemented. The new flow cookie is scoped to `/auth`, so it arrives after this same-origin redirect. The token exchange keeps the exact configured redirect URI, including a pre-existing legacy URI. |
| Failure page | `/app/login?sso=error`, `/app/error` | Signed, one-shot, 60-second HttpOnly error note at `/auth/session`; no code/state in the clean redirect; React escapes the verified message. |
| `GET /ui`, `GET /ui/` | 301 `/app/` | Bounded v0.20 bookmark shim. |
| `GET /ui/messages/{id}` | 301 `/app/messages/{id}` | Bounded v0.20 deep-message bookmark shim; actual mail access remains current-user scoped. |
| Other `/ui/*` | 404 | No login, admin, static or broad wildcard forwarding. The legacy handler import/mount is removed. |

New Keycloak/OIDC registrations should use `https://<postra-host>/auth/oidc/callback`. Existing persisted registrations are **not** silently rewritten: first add the new URI at the identity provider, then update the Postra setting and verify an interactive and silent login. Remove the legacy URI after the migration. The three bounded shims above are scheduled for removal at v1.

## Mail and work feature mapping

| Legacy route(s) | React / REST replacement | State / acceptance requirement |
| --- | --- | --- |
| `GET /ui/` | `/app/mail`, `/api/messages` | Keyword search, account/folder, sender, recipient, subject, label, date interval and attachment filters. Detailed filter UI, unread/snoozed folder and owner-scoped Unicode body snippets. Semantic/hybrid search is additional SPA functionality; clearly states that only account scope applies. |
| `POST /ui/messages/batch` | Inbox checkboxes + `/api/messages/batch` | Loaded-page selection, read/unread, archive/unarchive, importance on/off, snooze/unsnooze, labels, local delete. Partial failures remain selected, with explicit per-item error display. Delete requires confirmation and never deletes the IMAP/POP3 original. |
| `POST /ui/messages/{id}/action` | Detail toolbar + batch API | Importance, archive and local delete; multi-mail toolbar also offers snooze/labels. |
| `GET /ui/messages/{id}` | `/app/messages/{id}` and split detail | Opaque HTML sandbox, text fallback/toggle, metadata, attachments, reply/all/forward and AI tools. A separate inbound sanitizer retains inert external image metadata; admin HTML/image policy gates display. Per-user one-time/sender/domain image consent and revocation never mutate another user's trust. Unsafe data/SVG and recognizable tracking pixels are removed even after consent. |
| `GET /ui/threads/{id}` | `/app/threads/{id}`, `/api/threads/{id}/timeline` | Added oldest-first full conversation with direct single-message links, no pagination truncation; current-user scope remains in application/store. |
| `GET /ui/messages/{id}/attachments/{att}` | `/api/messages/{id}/attachments/{att}` | Clean download; suspicious/quarantined file requires explicit confirmation and `ack=true`; blocked file has no download action. No legacy detail link remains. |
| `POST /ui/messages/{id}/analyze` | AI Insight + `/api/messages/{id}/analyze` | Summary/triage plus added action_items, classify, entities and phishing selectors; structured escaped results. |
| `POST /ui/messages/{id}/suggest` | Smart reply + `/api/messages/{id}/suggest-replies` | Candidate selection only creates an AI-attributed draft. |
| `GET /ui/messages/{id}/calendar.ics` | AI Insight calendar preview + `/api/messages/{id}/calendar[.ics]` | Added explicit calendar extraction preview and ICS download; no external calendar write. |
| `POST /ui/messages/{id}/draft` | Reply buttons / AI instructions + `/api/drafts` | Reply, reply-all, forward, selected smart reply and freeform AI reply instructions. No automatic send. |
| `GET/POST /ui/compose` | `/app/compose`, `POST /api/drafts` | Existing rich-text compose; mailrender/signature owner supplies rendering/template extensions. |
| `GET/POST /ui/drafts/{id}` | `/app/drafts/{id}`, `GET/PATCH /api/drafts/{id}` | Versioned persistence, unsaved-edit protection, plain/HTML editors. |
| `POST /ui/drafts/{id}/delete` | `/app/drafts` delete, `DELETE /api/drafts/{id}` | Added server-backed list + deletion. SQLite/Postgres keyed pagination includes only the current user's current-version summary, never body/Bcc/approval credentials. Discarded audit records remain visible but unsendable. |
| `POST /ui/drafts/{id}/rewrite` | Compose rewrite + REST rewrite | Existing AI-authored version; rendering owner preserves new mail-format semantics. |
| `GET/POST /ui/drafts/{id}/send` | Compose preview/approval/send APIs | Exact payload hash, version and expiry bound approval; idempotent send; no implicit auto-send. |
| `GET /ui/outbound` | `/app/sent`, `/api/outbound` | SMTP outcome, attempts, retry time and uncertain-delivery warning. |
| `GET /ui/accounts`, `/new`, `/{id}` and POST create/update/delete/test/sync | `/app/accounts[/{id}]` + account/secrets APIs | Current-user account management; IMAP/POP3, TLS/STARTTLS/none, SMTP no-auth/shared-password, secret omission preserves saved values; tests and collection status. |
| `GET /ui/jobs/{id}` | `/app/jobs/{id}`, `/api/jobs/{id}` | Added full progress/stats/error page and cancellable active jobs. `/app/jobs` lists own recent jobs; account details retain inline polling. |
| `GET /ui/jobs/status` | Workspace notification menu + `/api/jobs` | Existing current-user background status; root connects direct job links. |
| `GET /ui/team`, `POST /ui/messages/{id}/collab` | `/app/team?message=id` + collab/assign/status/notes APIs | Current-user scoped team items, assignee/status filters, explicit processing panel and internal notes; SPA also supports SLA. Detail links to the panel. |
| `GET /ui/cards`, status POST, message cards POST | `/app/actions`, AI Insight + action-card APIs | Extract/review/approve/reject/done; SPA adds explicit export data (not an external write). |
| `GET /ui/digest` | `/app/digest`, `/api/digest` | Account/time window + briefing. |
| `GET /ui/rules`, draft/create/delete POST | `/app/rules`, rule APIs | AI suggestion review before save, deletion confirmation; SPA also supports simple manual creation and enable/disable. |

## Administration, assets and non-mail infrastructure

| Legacy route(s) | Replacement / owner | Required sign-off |
| --- | --- | --- |
| `/ui/mcp-keys`, create/revoke; `/ui/admin/mcp-keys/{id}/revoke` | `/app/keys`, admin key UI, REST/MCP scope owner | Current-user key lifecycle, one-time secret display, explicit scopes, revoke; administrator key operations remain audited. |
| `/ui/admin/users` + create/update/password/delete | Root admin UI + `/api/admin/users` | Local/OIDC users, disable/reactivate, reset/delete, no administrator mail-read bypass. |
| `/ui/admin/mail-provision` | Root admin UI + `/api/admin/mail-accounts` | Email-only mapping, optional target, SSO auto-provision, shared IMAP secret, optional SMTP no-auth. |
| `/ui/admin/deleted-mail/purge` | Root admin UI + `/api/admin/deleted-mail/purge` | Separate old-owner data management, exact email confirmation, preserve active user's private mail. |
| `/ui/admin/settings`, AI save/test, vector test | Root settings catalog/UI + configuration routes | Every effective settings source, secret write-only/redacted status, safe backend diagnostics, restart-required labeling. |
| `/ui/admin/incidents`, resolve | Root admin incident UI + incident APIs | Current active incident list and explicit resolution. |
| Tracking allow/forget POST and Momento proxy | `/app/admin/tracking`, `/api/tracking`, `/api/admin/tracking`, `/momento/` | Implemented without template dependency. Opt-in snippet runs only inside an opaque sandboxed nonce/CSP iframe on coarse whitelisted workspace routes, never login/setup or the parent app DOM. Violation reports remove queries/IDs; administrator allow/forget is CSRF-protected. Fixed-target proxy strips auth/cookies and upstream Set-Cookie. |
| `/ui/static/*`, favicon/logo, CSP-report endpoint | SPA embedded assets/security headers, root infrastructure | Remove legacy editor/template assets and template CSP-report dependency only after React rendering/monitoring replacements are verified. |

## Verification gates before deletion

- Auth transport: setup/local/token exact-origin tests; real signed mock OIDC ID token; legacy redirect URI preserved; silent refusal and signed error-note consume/tamper regression; safe return targets; admin/member isolation and CSRF.
- React auth: password/token forms, setup denial/password matching, escaped signed errors, silent SSO loop guards, transient-refresh editor preservation and identity-switch cache disposal.
- Drafts: current-owner-only listing, metadata-only response, timestamp/id cursor pagination, validation, persisted deletion, restart/fresh-query restoration; PostgreSQL adapter integration when `POSTRA_TEST_PG` is configured.
- Mail parity: filters survive pagination; select-only bulk mutation with cancel/partial-failure behavior; threads and jobs resolve owned data; extra AI actions are explicit; suspicious download requires acknowledgment; blocked never downloads.
- Read state: SQLite/PostgreSQL persist local `is_read`; opening a SPA message issues an explicit protected `mark_read` mutation once. REST GET is read-only. Initial migration marks old messages unread because historical per-user read state was not stored; IMAP server flags are deliberately unchanged.
- Reader preferences: effective server `ui.reader_position`, `ui.preview_lines`, `ui.ai_panel`, `search.default_mode`, `ai.auto_summary` and `ai.show_replies` drive the inbox/detail. Administrator locks disable user overrides; automatic AI summary is opt-in.
- Received HTML: default block and admin policy overrides; no active image source before explicit consent; trust is scoped to the current owner; persistent allow/revoke and one-time-only behavior; legacy raw MIME recovery is bounded to 5 MiB, in memory, after ownership validation, without rewriting stored bodies. External image loading may disclose the reader's IP/read activity; the UI warns before persistent trust. Pixel detection cannot identify every disguised tracking URL, so default blocking remains the primary privacy protection.
- Tracking/auth: isolated iframe has no same-origin permission; public CSP reports retain only coarse route/blocked origin; API tokens reject disabled local principals; inactive identities cannot keep notification streams authorized.
- Root final release gates: regenerate the synchronized final SPA bundle after all owners finish, rerun the actual-Go-server browser journeys and Go race/vet/security checks. Legacy removal itself was approved after the first complete browser gates passed.

Exact layout/wording need not match templates. Old broad `/ui` bookmarks other than root/message/callback are intentionally retired, not silently redirected. Drafts and read state are server-persisted and survive browser storage clearing. Outgoing rendering/signatures/attachments and the full administrative settings screens remain the respective owners' final sign-off gates, not missing mailbox parity work.

## Browser verification record (2026-09-15)

`TestSPAConvergenceBrowser` passed against the newly embedded SPA and real Go HTTP/application/SQLite paths (13.66 seconds). Its isolated localhost fixtures exercise first administrator setup, local login/logout, admin settings save and forced user theme, two-tab identity replacement, administrator/member mailbox isolation, an RSA-signed mock OIDC issuer with the new `/auth/oidc/callback` and clean deep return, and received-image privacy. The remote-image fixture observed zero requests before consent, exactly one after the explicit action, and no additional request on reload. A separate authenticated HTML frame applies its own opaque sandbox/CSP without widening the workspace's resource policy; one-time flags require a same-origin iframe navigation.

Mobile checks cover inbox, drafts, jobs, personal settings and logged-out forms at 390×844: no horizontal overflow, one main heading and named visible icon buttons. These are focused accessibility checks, not a claim of a complete WCAG audit. The setup password's accessible name/help are now separate.

The original explicit-SMTP-approval journey also passed (10.62 seconds): login/privacy, AI, Work/Actions, Q&A/digest, rich draft persistence, exact preview/approval, one captured fake SMTP delivery, mobile and cross-tab logout. Its selectors and fake AI routing were updated to the actual new `in_progress`, `qa`/`digest`/`compose` and template/format controls. Plain-text links/emphasis/XSS, browser-tampered rule JSON and oversized/incomplete tracking settings have dedicated React/HTTP regression coverage.

`TestSPADevProxyBrowser` also passed (13.42 seconds) using a real Vite development server and the Go auth/API: local login/logout, protected read-state mutation, rejected missing CSRF and the authenticated received-body iframe. The proxy forwards `/auth`, `/api`, `/tracking` and `/momento` while preserving the browser-visible Host; it no longer routes `/ui`.

The environment's installed browser was Chrome for Testing 151.0.7922.34 (`POSTRA_CHROME` override); the Playwright-pinned default binary was not installed. Reproduce with `cd web && npm run build && POSTRA_CHROME=/path/to/chrome npm run test:e2e`, or install the Playwright-managed browser and omit the override. `npm run test:e2e` runs the explicit-SMTP-approval, convergence and development-proxy journeys. Fixtures mount only the new transports and SPA; they no longer import the legacy webui package.

Mailbox/auth/settings-UI parity and approved template-package removal are complete. The remaining release gate is root/B/C final integrated verification and synchronized bundle/release artifacts, not retention of any legacy rendering implementation.
