import { afterEach, describe, expect, it, vi } from 'vitest'
import { act, cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react'
import type { Editor } from '@tiptap/react'
import { RichEditor } from './RichEditor'
import { safeMailStyle } from './MailExtensions'

// Deliberately render the real TipTap editor: a mocked textarea cannot expose
// programmatic update events emitted by editor.setEditable().
afterEach(cleanup)

describe('rich editor programmatic updates', () => {
  it('does not report user edits when disabled and re-enabled for saving', async () => {
    const onChange = vi.fn()
    const html = '<h2>저장할 서식 메일</h2><p>본문을 그대로 유지합니다.</p>'
    const { rerender } = render(<RichEditor value={html} disabled={false} onChange={onChange} />)
    const editor = await screen.findByRole('textbox', { name: '메일 본문 서식 편집기' })
    await waitFor(() => expect(editor).toHaveAttribute('contenteditable', 'true'))
    expect(onChange).not.toHaveBeenCalled()

    rerender(<RichEditor value={html} disabled onChange={onChange} />)
    await waitFor(() => expect(editor).toHaveAttribute('contenteditable', 'false'))
    expect(onChange).not.toHaveBeenCalled()

    rerender(<RichEditor value={html} disabled={false} onChange={onChange} />)
    await waitFor(() => expect(editor).toHaveAttribute('contenteditable', 'true'))
    expect(editor).toHaveTextContent('저장할 서식 메일')
    expect(editor).toHaveTextContent('본문을 그대로 유지합니다.')
    expect(onChange).not.toHaveBeenCalled()
  })

  it('does not report server-loaded HTML as an edit', async () => {
    const onChange = vi.fn()
    const { rerender } = render(<RichEditor value="<p>처음 내용</p>" onChange={onChange} />)
    await screen.findByRole('textbox', { name: '메일 본문 서식 편집기' })
    rerender(<RichEditor value="<h2>서버에 저장된 새 버전</h2>" onChange={onChange} />)
    await waitFor(() => expect(screen.getByRole('textbox', { name: '메일 본문 서식 편집기' })).toHaveTextContent('서버에 저장된 새 버전'))
    expect(onChange).not.toHaveBeenCalled()
  })

  it('still reports a real content change after re-enabling', async () => {
    const onChange = vi.fn()
    const { rerender } = render(<RichEditor value="<p>기존 본문</p>" disabled onChange={onChange} />)
    const element = await screen.findByRole('textbox', { name: '메일 본문 서식 편집기' })
    rerender(<RichEditor value="<p>기존 본문</p>" disabled={false} onChange={onChange} />)
    const instance = (element as HTMLElement & { editor: Editor }).editor
    act(() => { instance.commands.insertContent('사용자 변경 ') })
    expect(onChange).toHaveBeenCalledOnce()
    expect(onChange.mock.calls[0][0]).toContain('사용자 변경')
  })

  it('updates toolbar selection state without dirtying the body', async () => {
    const onChange = vi.fn()
    render(<RichEditor value="<p><strong>굵은 문장</strong> 일반 문장</p>" onChange={onChange} />)
    const element = await screen.findByRole('textbox', { name: '메일 본문 서식 편집기' })
    const instance = (element as HTMLElement & { editor: Editor }).editor
    act(() => { instance.commands.setTextSelection(3) })
    await waitFor(() => expect(screen.getByRole('button', { name: '굵게' })).toHaveAttribute('aria-pressed', 'true'))
    act(() => { instance.commands.setTextSelection(10) })
    await waitFor(() => expect(screen.getByRole('button', { name: '굵게' })).toHaveAttribute('aria-pressed', 'false'))
    expect(onChange).not.toHaveBeenCalled()
  })

  it('preserves renderer and signature provenance through a real edit', async () => {
    const onChange = vi.fn()
    render(<RichEditor value='<div data-postra-template="report" style="padding:24px;border:1px solid #dfe5ee"><p>보고 본문</p><div data-postra-signature="sig_one"><p>홍길동</p></div></div>' onChange={onChange} />)
    const element = await screen.findByRole('textbox', { name: '메일 본문 서식 편집기' })
    const instance = (element as HTMLElement & { editor: Editor }).editor
    act(() => { instance.commands.insertContentAt(2, '새 ') })
    const html = onChange.mock.calls[0][0]
    expect(html).toContain('data-postra-template="report"')
    expect(html).toContain('data-postra-signature="sig_one"')
    expect(html).toContain('padding: 24px')
    expect(html).not.toContain(' template="') // internal attribute names are not serialized
  })

  it('offers a selected-text proposal and changes only that range after explicit apply', async () => {
    const onChange = vi.fn()
    const rewrite = vi.fn(async () => '<img src=x> 안전한 제안')
    render(<RichEditor value="<p>선택한 문장</p><p>유지할 뒷문장</p>" onChange={onChange} onRewriteSelection={rewrite} />)
    const element = await screen.findByRole('textbox', { name: '메일 본문 서식 편집기' })
    const instance = (element as HTMLElement & { editor: Editor }).editor
    act(() => { instance.commands.setTextSelection({ from: 1, to: 7 }) })
    fireEvent.click(screen.getByRole('button', { name: '선택 영역 다듬기' }))
    await screen.findByRole('button', { name: '선택 영역에 적용' })
    expect(rewrite).toHaveBeenCalledWith('선택한 문장')
    expect(onChange).not.toHaveBeenCalled()
    fireEvent.click(screen.getByRole('button', { name: '선택 영역에 적용' }))
    expect(onChange).toHaveBeenCalledOnce()
    expect(onChange.mock.calls[0][0]).toContain('&lt;img src=x&gt;')
    expect(onChange.mock.calls[0][0]).toContain('유지할 뒷문장')
  })

  it('never mounts network-capable or active authored CSS', () => {
    const style = safeMailStyle('color:red;background-image:url(https://tracker.test/x);position:fixed;--foo:red;font-family:var(--foo);padding:12px')
    expect(style).toContain('color: red')
    expect(style).toContain('padding: 12px')
    expect(style).not.toMatch(/url|tracker|position|var\(|--foo/)
  })
})
