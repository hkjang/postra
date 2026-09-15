import { afterEach, describe, expect, it, vi } from 'vitest'
import { createWorkspaceShortcuts } from './workspace-shortcuts'

afterEach(() => { document.body.replaceChildren() })
function setup() {
  const actions = { navigate: vi.fn(), openCommand: vi.fn(), showHelp: vi.fn(), closePanels: vi.fn(() => false), mail: vi.fn((_command: string) => true), now: vi.fn(() => 1000) }
  const shortcuts = createWorkspaceShortcuts(actions)
  function press(key: string, options: KeyboardEventInit = {}, target: Element = document.body) {
    const event = new KeyboardEvent('keydown', { key, bubbles: true, cancelable: true, ...options })
    target.addEventListener('keydown', shortcuts.keydown as EventListener, { once: true }); target.dispatchEvent(event)
    return event
  }
  return { actions, shortcuts, press }
}
describe('workspace keyboard boundaries', () => {
  it('maps mail commands without creating approval, send, or destructive delete actions', () => {
    const { actions, press } = setup()
    for (const key of ['r', 'a', 'f', 'e', 'j', 'k', 'Escape']) expect(press(key).defaultPrevented).toBe(true)
    expect(actions.mail.mock.calls.map(([command]) => command)).toEqual(['reply', 'reply_all', 'forward', 'archive', 'next', 'previous', 'close'])
    press('Delete'); press('Enter', { ctrlKey: true }); expect(actions.mail).toHaveBeenCalledTimes(7)
    actions.mail.mockReturnValue(false); expect(press('r').defaultPrevented).toBe(false)
  })
  it('honors ordered G I/G W, expires the prefix and preserves ordinary navigation', () => {
    const { actions, press } = setup()
    press('g'); press('i'); press('g'); press('w'); press('c')
    expect(actions.navigate.mock.calls).toEqual([['/mail'], ['/work'], ['/compose']])
    press('g'); actions.now.mockReturnValue(2500); press('i')
    press('g'); press('x'); press('w')
    expect(actions.navigate).toHaveBeenCalledTimes(3)
    press('/'); press('k', { ctrlKey: true }); press('k', { metaKey: true }); press('?')
    expect(actions.openCommand).toHaveBeenCalledTimes(3); expect(actions.showHelp).toHaveBeenCalledOnce()
  })
  it('ignores text fields, nested rich editing, focused input and all IME variants', () => {
    const { actions, shortcuts, press } = setup()
    for (const tag of ['input', 'textarea', 'select']) {
      const input = document.createElement(tag); document.body.append(input)
      press('r', {}, input); press('k', { ctrlKey: true }, input)
    }
    const editor = document.createElement('div'); editor.setAttribute('contenteditable', 'true')
    const span = document.createElement('span'); editor.append(span); document.body.append(editor); press('e', {}, span)
    const input = document.createElement('input'); document.body.append(input); input.focus(); press('f'); input.blur()
    press('r', { isComposing: true }); press('r', { keyCode: 229 }); press('r', { repeat: true })
    shortcuts.compositionStart(); press('r'); shortcuts.compositionEnd()
    expect(actions.mail).not.toHaveBeenCalled(); expect(actions.openCommand).not.toHaveBeenCalled()
    press('g'); shortcuts.compositionStart(); shortcuts.compositionEnd(); press('i'); expect(actions.navigate).not.toHaveBeenCalled()
  })
  it('does not steal modal keys or modified browser shortcuts and closes the mobile menu first', () => {
    const { actions, press } = setup()
    const dialog = document.createElement('section'); dialog.setAttribute('role', 'alertdialog'); document.body.append(dialog)
    press('r'); press('Escape'); press('k', { ctrlKey: true }); expect(actions.mail).not.toHaveBeenCalled(); expect(actions.openCommand).not.toHaveBeenCalled()
    dialog.remove(); press('r', { ctrlKey: true }); press('f', { metaKey: true }); press('e', { altKey: true })
    expect(actions.mail).not.toHaveBeenCalled()
    actions.closePanels.mockReturnValue(true); press('Escape'); expect(actions.mail).not.toHaveBeenCalled()
  })
})
