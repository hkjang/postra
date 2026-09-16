import type { Account, ActionCard, Attachment, Message, MessageView } from './types'
import {InvalidResponseError} from '@/api/response'

const invalid = () => new InvalidResponseError()
export function responseObject(value: unknown): Record<string, unknown> {
  if (!value || typeof value !== 'object' || Array.isArray(value)) throw invalid()
  return value as Record<string, unknown>
}
export function responseList<T>(value: unknown, parse: (value: unknown) => T): T[] {
  if (value === null) return [] // older Go responses encoded empty slices as null
  if (!Array.isArray(value)) throw invalid()
  return value.map(parse)
}
export function responseString(value: unknown): string {
  if (typeof value !== 'string') throw invalid()
  return value
}
function optionalString(value: unknown): string | undefined { return value == null ? undefined : responseString(value) }
function optionalNumber(value: unknown): number | undefined {
  if (value == null) return undefined
  if (typeof value !== 'number' || !Number.isFinite(value)) throw invalid()
  return value
}
function optionalBoolean(value: unknown): boolean | undefined {
  if (value == null) return undefined
  if (typeof value !== 'boolean') throw invalid()
  return value
}
function address(value: unknown) {
  const object = responseObject(value)
  return {email: responseString(object.email), name: optionalString(object.name)}
}
export function messageResponse(value: unknown): Message {
  const row = responseObject(value)
  const id = responseString(row.id), account_id = responseString(row.account_id)
  if (!id || !account_id) throw invalid()
  return {id, account_id, subject: optionalString(row.subject) ?? '', from: address(row.from),
    to: responseList(row.to ?? null, address), cc: responseList(row.cc ?? null, address),
    date: optionalNumber(row.date) ?? 0, created_at: optionalNumber(row.created_at) ?? 0,
    has_attachments: optionalBoolean(row.has_attachments) ?? false,
    is_read: optionalBoolean(row.is_read), is_important: optionalBoolean(row.is_important), is_archived: optionalBoolean(row.is_archived),
    labels: responseList(row.labels ?? null, responseString), thread_id: optionalString(row.thread_id), auth_results: optionalString(row.auth_results), parse_error: optionalString(row.parse_error)}
}
export function messageViewResponse(value: unknown): MessageView {
  const row = responseObject(value)
  const body = row.body == null ? undefined : responseObject(row.body)
  return {message: messageResponse(row.message), body: body && {
    text_body: optionalString(body.text_body) ?? '', html_sanitized: optionalString(body.html_sanitized),
    unavailable: optionalBoolean(body.unavailable), unavailable_reason: optionalString(body.unavailable_reason),
    external_images: optionalNumber(body.external_images), images_allowed: optionalBoolean(body.images_allowed), image_policy: optionalString(body.image_policy),
    image_sender_trusted: optionalBoolean(body.image_sender_trusted), image_domain_trusted: optionalBoolean(body.image_domain_trusted)},
    attachments: responseList(row.attachments ?? null, value => {
      const item = responseObject(value)
      return {id: responseString(item.id), name: responseString(item.name), size: optionalNumber(item.size) ?? 0, scan_status: responseString(item.scan_status), scan_detail: optionalString(item.scan_detail)} satisfies Attachment
    }), score: optionalNumber(row.score), reason: optionalString(row.reason)}
}
export function messagePageResponse(value: unknown) {
  const row = responseObject(value)
  const snippets = row.snippets == null ? {} : responseObject(row.snippets)
  return {messages: responseList(row.messages, messageResponse), next_cursor: optionalString(row.next_cursor),
    snippets: Object.fromEntries(Object.entries(snippets).map(([id, text]) => [id, responseString(text)]))}
}
export function accountListResponse(value: unknown): Account[] {
  return responseList(value, value => { const item = responseObject(value); return {id: responseString(item.id), name: responseString(item.name), email: responseString(item.email), status: optionalString(item.status) ?? ''} })
}
export function actionCardsResponse(value: unknown): {cards: ActionCard[]} {
  return {cards: responseList(responseObject(value).cards, value => {
    const item = responseObject(value)
    return {id: responseString(item.id), title: responseString(item.title), status: responseString(item.status), due: optionalString(item.due), detail: optionalString(item.detail), assignee: optionalString(item.assignee), confidence: optionalNumber(item.confidence)}
  })}
}
export function repliesResponse(value: unknown) { return {suggestions: responseList(responseObject(value).suggestions, responseString).filter(text => text.trim())} }
export function calendarResponse(value: unknown) {
  return {events: responseList(responseObject(value).events, value => { const item = responseObject(value); return {title: responseString(item.title), start: responseString(item.start), location: optionalString(item.location)} })}
}
export function batchResponse(value: unknown) {
  const row = responseObject(value)
  const failed = optionalNumber(row.failed), succeeded = optionalNumber(row.succeeded)
  if (failed === undefined || failed < 0 || !Number.isInteger(failed) || (succeeded !== undefined && (succeeded < 0 || !Number.isInteger(succeeded)))) throw invalid()
  const results = responseList(row.results, value => {const item = responseObject(value); return {message_id: responseString(item.message_id), ok: optionalBoolean(item.ok) === true, error: optionalString(item.error)}})
  // Missing item outcomes must never clear the user's selected mail or claim
  // success after a malformed partial result. A zero-count response is valid.
  if ((failed > 0 || (succeeded ?? 0) > 0) && !results.length) throw invalid()
  return {failed, succeeded: succeeded ?? 0, results}
}

// Reply creation only needs a navigation target. Do not populate another
// feature's draft cache with an unvalidated creation response; it refetches.
export function createdDraftID(value: unknown): string {
  const id = responseString(responseObject(responseObject(value).draft).id)
  if (!id.trim()) throw invalid()
  return id
}
