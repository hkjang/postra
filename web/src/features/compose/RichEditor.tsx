import { useEffect, useState } from 'react'
import { EditorContent, useEditor } from '@tiptap/react'
import StarterKit from '@tiptap/starter-kit'
import TextAlign from '@tiptap/extension-text-align'
import { TextStyle } from '@tiptap/extension-text-style'
import Color from '@tiptap/extension-color'
import FontFamily from '@tiptap/extension-font-family'
import { Table, TableCell, TableHeader, TableRow } from '@tiptap/extension-table'
import { AlignCenter, AlignLeft, Bold, Italic, Link2, List, ListOrdered, Quote, Redo2, RemoveFormatting, Sparkles, Table2, Underline, Undo2 } from 'lucide-react'
import { Button, Input } from '@/components/ui'
import { safeLink } from './utils'
import { MailContainer, MailStyles, MailImage } from './MailExtensions'

export function RichEditor({ value, onChange, disabled, onRewriteSelection }: { value: string; onChange: (html: string, text: string) => void; disabled?: boolean; onRewriteSelection?: (text: string) => Promise<string> }) {
  const [linkOpen, setLinkOpen] = useState(false)
  const [link, setLink] = useState('')
  const [linkError, setLinkError] = useState('')
  const [suggestion, setSuggestion] = useState<{ from: number; to: number; before: string; after: string }>()
  const [rewriteError, setRewriteError] = useState('')
  const editor = useEditor({
    // TipTap 3 defaults to no transaction-driven React rerenders. Selection
    // changes must still refresh pressed marks and the contextual table tools.
    shouldRerenderOnTransaction: true,
    extensions: [
      MailContainer, MailStyles, MailImage,
      StarterKit.configure({
        link: { openOnClick: false, protocols: ['http', 'https', 'mailto'], HTMLAttributes: { rel: 'noopener noreferrer nofollow', style: 'color: #3157d5; text-decoration: underline' }, isAllowedUri: url => safeLink(url) },
        paragraph: { HTMLAttributes: { style: 'font-family: Arial, sans-serif; line-height: 1.7; margin: 0 0 12px' } },
        heading: { levels: [1, 2, 3], HTMLAttributes: { style: 'font-family: Arial, sans-serif; color: #3157d5; line-height: 1.4; margin: 18px 0 12px' } },
        blockquote: { HTMLAttributes: { style: 'border-left: 3px solid #b8c6f5; margin-left: 0; padding-left: 16px; color: #596479' } },
        horizontalRule: { HTMLAttributes: { style: 'border: 0; border-top: 1px solid #dfe5ee; margin: 18px 0' } },
      }),
      TextAlign.configure({ types: ['heading', 'paragraph'] }), TextStyle, Color, FontFamily,
      Table.configure({ resizable: false, HTMLAttributes: { style: 'border-collapse: collapse; width: 100%; margin: 12px 0' } }),
      TableRow, TableCell.configure({ HTMLAttributes: { style: 'border: 1px solid #dfe5ee; padding: 12px' } }),
      TableHeader.configure({ HTMLAttributes: { style: 'border: 1px solid #dfe5ee; padding: 12px; background-color: #eef2ff; text-align: left' } }),
    ],
    content: value || '<p></p>',
    editable: !disabled,
    editorProps: { attributes: { class: 'rich-editor-content', role: 'textbox', 'aria-label': '메일 본문 서식 편집기', 'aria-multiline': 'true' } },
    onUpdate: ({ editor: instance }) => onChange(instance.getHTML(), instance.getText()),
  })
  useEffect(() => { if (editor && editor.getHTML() !== value) editor.commands.setContent(value || '<p></p>', { emitUpdate: false }) }, [editor, value])
  // Busy/idle is presentation state, not a user-authored revision. TipTap's
  // default emits an update here, which would dirty a just-saved draft and
  // invalidate the approval preview merely by re-enabling the editor.
  useEffect(() => { editor?.setEditable(!disabled, false) }, [editor, disabled])
  if (!editor) return null
  const commands = [
    { label: '굵게', icon: Bold, active: editor.isActive('bold'), run: () => editor.chain().focus().toggleBold().run() },
    { label: '기울임', icon: Italic, active: editor.isActive('italic'), run: () => editor.chain().focus().toggleItalic().run() },
    { label: '밑줄', icon: Underline, active: editor.isActive('underline'), run: () => editor.chain().focus().toggleUnderline().run() },
    { label: '글머리 목록', icon: List, active: editor.isActive('bulletList'), run: () => editor.chain().focus().toggleBulletList().run() },
    { label: '번호 목록', icon: ListOrdered, active: editor.isActive('orderedList'), run: () => editor.chain().focus().toggleOrderedList().run() },
    { label: '인용', icon: Quote, active: editor.isActive('blockquote'), run: () => editor.chain().focus().toggleBlockquote().run() },
    { label: '왼쪽 정렬', icon: AlignLeft, active: editor.isActive({ textAlign: 'left' }), run: () => editor.chain().focus().setTextAlign('left').run() },
    { label: '가운데 정렬', icon: AlignCenter, active: editor.isActive({ textAlign: 'center' }), run: () => editor.chain().focus().setTextAlign('center').run() },
  ]
  function addLink() {
    const url = link.trim()
    if (!url) { editor?.chain().focus().unsetLink().run(); setLinkOpen(false); return }
    if (!safeLink(url)) { setLinkError('http://, https://, mailto: 주소만 사용할 수 있습니다.'); return }
    editor?.chain().focus().extendMarkRange('link').setLink({ href: url }).run()
    setLinkOpen(false); setLinkError('')
  }
  async function rewriteSelection() {
    if (!editor || !onRewriteSelection || editor.state.selection.empty) return
    const { from, to } = editor.state.selection
    const before = editor.state.doc.textBetween(from, to, '\n')
    setRewriteError(''); setSuggestion(undefined)
    try { const after = await onRewriteSelection(before); setSuggestion({ from, to, before, after }) }
    catch (error) { setRewriteError(error instanceof Error ? error.message : '선택 영역을 다듬지 못했습니다.') }
  }
  function applySuggestion() {
    if (!suggestion) return
    if (editor.state.doc.textBetween(suggestion.from, suggestion.to, '\n') !== suggestion.before) { setRewriteError('선택한 원문이 변경되었습니다. 다시 선택해 주세요.'); setSuggestion(undefined); return }
    // Insert a text node, never provider-generated markup.
    editor.chain().focus().insertContentAt({ from: suggestion.from, to: suggestion.to }, { type: 'text', text: suggestion.after }).run()
    setSuggestion(undefined)
  }
  return <div className="rich-editor">
    <div className="compose-toolbar" role="toolbar" aria-label="메일 서식 도구">
      <select aria-label="문단 서식" disabled={disabled} value={editor.isActive('heading', { level: 2 }) ? 'h2' : editor.isActive('heading', { level: 3 }) ? 'h3' : 'p'} onChange={event => event.target.value === 'p' ? editor.chain().focus().setParagraph().run() : editor.chain().focus().toggleHeading({ level: event.target.value === 'h2' ? 2 : 3 }).run()}><option value="p">본문</option><option value="h2">큰 제목</option><option value="h3">작은 제목</option></select>
      <select aria-label="글꼴" disabled={disabled} value={editor.getAttributes('textStyle').fontFamily || ''} onChange={event => event.target.value ? editor.chain().focus().setFontFamily(event.target.value).run() : editor.chain().focus().unsetFontFamily().run()}><option value="">기본 글꼴</option><option value="Arial, sans-serif">고딕</option><option value="Georgia, serif">명조</option><option value="monospace">고정폭</option></select>
      {commands.map(command => <Button key={command.label} size="icon" variant={command.active ? 'secondary' : 'ghost'} disabled={disabled} aria-label={command.label} aria-pressed={command.active} title={command.label} onClick={command.run}><command.icon size={16} /></Button>)}
      <label className="editor-color" title="글자색"><span className="sr-only">글자색</span><input type="color" aria-label="글자색" disabled={disabled} defaultValue="#3157d5" onInput={event => editor.chain().focus().setColor(event.currentTarget.value).run()} /></label>
      <Button size="icon" variant="ghost" disabled={disabled} aria-label="링크 설정" title="링크 설정" onClick={() => { setLink(editor.getAttributes('link').href || ''); setLinkError(''); setLinkOpen(value => !value) }}><Link2 size={16} /></Button>
      <Button size="icon" variant="ghost" disabled={disabled} aria-label="표 삽입" title="표 삽입" onClick={() => editor.chain().focus().insertTable({ rows: 3, cols: 2, withHeaderRow: true }).run()}><Table2 size={16} /></Button>
      <Button size="icon" variant="ghost" disabled={disabled} aria-label="서식 지우기" title="서식 지우기" onClick={() => editor.chain().focus().unsetAllMarks().clearNodes().run()}><RemoveFormatting size={16} /></Button>
      <Button size="icon" variant="ghost" disabled={disabled || !editor.can().undo()} aria-label="실행 취소" title="실행 취소" onClick={() => editor.chain().focus().undo().run()}><Undo2 size={16} /></Button>
      <Button size="icon" variant="ghost" disabled={disabled || !editor.can().redo()} aria-label="다시 실행" title="다시 실행" onClick={() => editor.chain().focus().redo().run()}><Redo2 size={16} /></Button>
      {onRewriteSelection && <Button size="sm" variant="ghost" disabled={disabled || editor.state.selection.empty} onClick={rewriteSelection}><Sparkles size={15} /> 선택 영역 다듬기</Button>}
    </div>
    {linkOpen && <div className="compose-link-form"><Input aria-label="링크 주소" value={link} placeholder="https:// 또는 mailto:" onChange={event => setLink(event.target.value)} onKeyDown={event => { if (event.key === 'Enter') { event.preventDefault(); addLink() } }} /><Button size="sm" onClick={addLink}>적용</Button><Button size="sm" variant="ghost" onClick={() => setLinkOpen(false)}>취소</Button>{linkError && <p role="alert">{linkError}</p>}</div>}
    <EditorContent editor={editor} />
    {rewriteError && <p role="alert" className="preview-warning">{rewriteError}</p>}
    {suggestion && <section className="compose-selection-result" aria-label="선택 영역 AI 제안"><h3>선택한 문장 다듬기</h3><p className="muted">선택하지 않은 본문은 변경하지 않습니다.</p><pre>{suggestion.after}</pre><div className="row"><Button size="sm" disabled={disabled} onClick={applySuggestion}>선택 영역에 적용</Button><Button size="sm" variant="ghost" onClick={() => setSuggestion(undefined)}>취소</Button></div></section>}
    {editor.isActive('table') && <div className="compose-table-tools"><Button variant="ghost" size="sm" disabled={disabled} onClick={() => editor.chain().focus().addRowAfter().run()}>행 추가</Button><Button variant="ghost" size="sm" disabled={disabled} onClick={() => editor.chain().focus().addColumnAfter().run()}>열 추가</Button><Button variant="ghost" size="sm" disabled={disabled} onClick={() => editor.chain().focus().deleteTable().run()}>표 삭제</Button></div>}
  </div>
}
