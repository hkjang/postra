import { afterEach, describe, expect, it, vi } from 'vitest'
import { act, cleanup, render, screen, waitFor } from '@testing-library/react'
import type { Editor } from '@tiptap/react'
import { RichEditor } from './RichEditor'

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
})
