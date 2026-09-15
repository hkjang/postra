# Shared mail rendering, signatures, attachments, and keyboard workflow

The Web composer and MCP use the same application renderer and draft/send pipeline. Rendering is not sending, and no keyboard shortcut or AI rewrite requests a send approval automatically.

## Rendering contract

`POST /api/mail/render` / MCP `mail_render` accept `account_id`, `body`, `body_html`, `format`, `template`, `use_signature`, `signature_id`, `intent`, `tone`, `length`, `language`, `reply_to_message_id`, and the optional `smart_format` flag. Draft create/update expose the same rendering options. The renderer returns canonical `body_html` and `body_text`, the selected format/template, and warnings.

- Formats: `text`, `markdown`, `html`, `auto`. Explicit `auto` detects supported HTML/Markdown or escapes text; it never requires an LLM. Old REST callers omitting all rendering options retain their prior behavior. New Web and MCP draft creation select `auto` explicitly.
- Templates: `clean`, `formal`, `concise`, `notice`, `report`, `newsletter`, `plain`. The six rich templates use inline CSS and local assets only. Applying another template replaces renderer-owned styles while preserving safe author styles. `plain` produces text only.
- Markdown is a deliberately bounded mail-oriented subset: paragraphs, headings, lists, quotes, fenced code, emphasis, links, and tables. It is not a complete CommonMark implementation. Raw HTML in Markdown is escaped.
- HTML passes an allowlist sanitizer: no scripts, event handlers, forms, embedded documents, SVG/data URLs, or CSS resource URLs. Plain text and DLP input derive from the sanitized canonical HTML, not a caller-supplied alternative that could hide HTML contents.
- Personal and account preferences are layered with administrator policy. Locked values override explicit render options. `mail.html_enabled=false` produces a plain rendering and prevents sending an existing HTML draft until it is saved again in an allowed form.
- `smart_format=true` uses the existing shared AI policy/model/audit path only when explicitly requested. MCP needs `mail.ai` as well as draft permission. Selected-text rewriting returns a proposal, does not add signatures, and is inserted only after the user applies it to the unchanged selected range.

## Signatures

`/api/signatures` and corresponding `mail_signature_*` tools operate only on the current user's signatures. An administrator cannot read or change another user's signature through these personal APIs. An optional account restriction is checked for both storage and application.

Signatures support a name, rich/text content, and optional structured display name, title, department, company, phone, email, and logo URL. With empty content, the structured fields generate escaped content. Remote logos follow outbound image policy; the editor and local preview do not load them.

The `smart` signature policy uses full signatures for new messages, a compact name/company/email signature for a first reply, and omits a signature only when a previous sent reply in the same owned conversation can be proven. Unstructured signatures fall back to their first three text lines; missing history conservatively counts as a first reply. Rich renderer-owned signature markers support replacement without duplication. Plain-text duplicate removal is intentionally limited to an exact known signature suffix, rather than guessing at and deleting user-authored footer text.

## Outgoing image policy

`mail.outbound_images` is `block` (default), `allow`, or `proxy`. `proxy` requires an operator-configured HTTPS `mail.image_proxy_url`; Postra constructs a URL with a URL-encoded `url` query parameter. Rendering never fetches the original or proxy URL, so this setting does not create a server-side fetch/SSRF path. CID attachment images remain supported under all three modes.

Editor node views show image placeholders. Local HTML previews block remote image requests even when outbound policy permits the recipient to load them. Sanitized remote images explicitly authorized by the renderer carry an allow/proxy marker; historical unmarked remote images retain the previous stripping behavior. Preview, immediate send, and delayed delivery recheck current policy. Tightening policy requires rendering and approving a new draft version; an approved payload is never silently rewritten at send time.

## Attachments and SMTP MIME

Web uploads use `POST /api/drafts/{id}/attachments` (base64 JSON or one multipart `file`); inline image uploads use `inline=true`. MCP supports add, metadata/download reference, optional bounded base64 retrieval, and removal. All routes and application methods verify the draft owner's account before reading or modifying files, including historical-version downloads.

- Uploads check administrator count/size/extension/scanner policy and a hard 64 MiB per-file ceiling. Raster inline images must decode as PNG, JPEG, or GIF and remain within the pixel limit; SVG and mislabeled image bytes are rejected.
- Each addition/removal creates an immutable draft version. The descriptor and content hash participate in approval payload hashing. Content is stored through the encrypted object store, and send rechecks descriptor consistency, bytes, hash, and current scan policy. Removing an attachment from a new version intentionally retains older-version data.
- MIME uses `multipart/alternative` for text + HTML, `multipart/related` for CID images, and outer `multipart/mixed` for ordinary files. Filenames use MIME encoding, binary content uses wrapped Base64, and a Unicode filename plus exact inline bytes are round-trip tested.
- DLP examines canonical mail content, filenames, and supported textual attachments. Binary attachments receive configured malware scanning but **not** automatic PDF/Office extraction or OCR for outbound DLP; the preview explicitly warns about this limitation.
- Preview does not grant approval. After review, explicit approval binds the version/payload; sending consumes that approval and uses idempotency protections. Editing, rerendering, or changing attachments invalidates an older approval. There is no shortcut to approve or send.

## Keyboard and MCP context

| Key | Action |
| --- | --- |
| C | New draft |
| / or Ctrl/Cmd K | Global search/command palette |
| G then I / G then W | Inbox / My Work (within 1.2 seconds) |
| J / K | Next / previous already-loaded result, with virtual list scrolling |
| R / A / F | Reply / reply all / forward draft for the open message |
| E | Archive the open message; not delete or unarchive |
| Esc | Close mobile navigation or the open detail; modals own their Escape behavior |
| ? | Shortcut help |

Typing fields, rich editors, Korean IME composition (including legacy key code 229), repeated keys, browser modifier shortcuts, and open dialogs suppress mail shortcuts. Shortcuts do not intercept keys inside the isolated HTML body frame: focus the message toolbar/list to use them. Only mounted, successfully authorized message/list views register commands; navigation removes registrations. List navigation never invents IDs or searches outside the loaded query result. The command palette exposes only available current-mail operations, and the admin shortcut is shown only to an admin principal.

The detail's **MCP Context** action previews and explicitly copies a JSON reference with account/message/thread IDs and `mail_message_get` / reply-draft examples. It excludes body, subject, personal addresses, attachment contents, credentials, and approval tokens. The MCP caller must independently authenticate and pass ownership/scope checks. A blocked clipboard falls back to selectable text; copying never calls MCP, sends data to an external service, or sends a message.

## Verification and scope

Run targeted server tests:

```sh
go test ./internal/mailrender ./internal/platform/mailhtml ./internal/application ./internal/transport/httpapi
go test ./internal/adapters/persistence ./internal/adapters/pgstore
```

Run frontend verification (no deployment build required):

```sh
cd web
npm run typecheck
npm test -- src/features/compose src/features/messages src/features/inbox src/lib/workspace-shortcuts.test.ts src/lib/mail-commands.test.tsx
```

`internal/mailrender/testdata` holds exact HTML/Markdown/text fragment fixtures and a styled clean-template HTML golden. Template tests cover every preset, deterministic reapplication, style preservation/replacement, signature handling, and sanitizer boundaries. `internal/application/testdata/rendered-mail.golden.eml` is a normalized SMTP MIME golden; Date and random MIME boundary values are replaced only in the test comparison. Application tests additionally inspect the captured SMTP bytes, nested multipart structure, Unicode filename, inline image contents, ownership, DLP, immutable approval, idempotent replay, and policy changes between preview and send. SQLite/PostgreSQL adapter tests cover attachment-version persistence/isolation; runtime PostgreSQL availability is required for its integration cases.

`TestSMTPIntegrationRenderedAttachmentApprovalAndIdempotency` also exercises the production SMTP adapter over a real loopback TCP SMTP conversation. It verifies no-AUTH submission despite an advertised AUTH capability, Markdown → HTML/plain multipart with an attachment, Bcc header omission, the approval path, and one DATA transfer for an idempotent replay. Dropping the final DATA response produces an uncertain outbound state without an automatic duplicate delivery.

These tests do not contact a live organization relay or a matrix of email clients. They establish wire structure, local SMTP protocol behavior, and policy invariants; they do not claim pixel-identical Outlook/Gmail rendering, delivery through an actual offline SMTP relay, external proxy reachability, or binary-file content DLP. Before deployment, send one text, one rich template, one Unicode attachment, and one CID-image mail through the organization's real relay and inspect both display and raw MIME with its supported mail clients.
