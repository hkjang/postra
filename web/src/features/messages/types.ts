export interface Address { name?: string; email: string }
export interface Message {
  id: string; account_id: string; subject: string; from: Address; to?: Address[]; cc?: Address[];
  date: number; created_at: number; has_attachments: boolean; is_important?: boolean;
  is_archived?: boolean; labels?: string[]; thread_id?: string;
}
export interface Attachment { id: string; name: string; size: number; scan_status: string; scan_detail?: string }
export interface MessageView {
  message: Message;
  body?: { text_body: string; html_sanitized?: string; unavailable?: boolean; unavailable_reason?: string };
  attachments?: Attachment[]; score?: number; reason?: string;
}
export interface Account { id: string; name: string; email: string; status: string; smtp_host?: string }
export interface Analysis { id: string; result_json: string; model: string }
export interface ActionCard { id: string; title: string; detail?: string; due?: string; assignee?: string; status: string; confidence?: number }
export interface DraftView {
  draft: { id: string; account_id: string; status: string; current_version: number; updated_at: number };
  version: { version: number; subject: string; to: Address[]; cc?: Address[]; bcc?: Address[]; body_text: string; body_html?: string; author: string };
}
export interface SendPreview {
  draft_id: string; draft_version: number; from: string; to: string[]; cc?: string[]; bcc?: string[];
  subject: string; body: string; body_html?: string; external_domains?: string[]; recipient_count: number;
  warnings?: string[]; payload_hash: string; dlp_blocked?: boolean;
  dlp_findings?: { type: string; count: number; term?: string }[];
}
export function addresses(values?: Address[]): string { return (values ?? []).map(x => x.name ? `${x.name} <${x.email}>` : x.email).join(', ') }
export function mailDate(unix: number): string { return unix ? new Intl.DateTimeFormat('ko-KR', { month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit' }).format(new Date(unix * 1000)) : '날짜 없음' }
