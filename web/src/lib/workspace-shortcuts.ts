import type { MailCommand } from './mail-commands'

type Actions = {
  navigate: (path: string) => void
  openCommand: () => void
  showHelp: () => void
  closePanels: () => boolean
  mail: (command: MailCommand) => boolean
  now?: () => number
}
const editingSelector = 'input,textarea,select,[role="textbox"],[role="combobox"],[contenteditable]:not([contenteditable="false"])'
export function isTypingTarget(target: EventTarget | null): boolean {
  return target instanceof Element && (!!target.closest(editingSelector) || target instanceof HTMLElement && target.isContentEditable)
}

// G I / G W are short sequences, not chords. No key is synthesized, no action is
// repeated, and Korean IME composition (including keyCode 229) always wins.
export function createWorkspaceShortcuts(actions: Actions) {
  let prefixAt: number | undefined
  let composing = false
  const reset = () => { prefixAt = undefined }
  return {
    compositionStart() { composing = true; reset() },
    compositionEnd() { composing = false; reset() },
    reset,
    keydown(event: KeyboardEvent) {
      if (event.defaultPrevented || event.repeat || composing || event.isComposing || event.keyCode === 229 || isTypingTarget(event.target) || isTypingTarget(document.activeElement)) { reset(); return }
      // Modal components own their keyboard/focus behavior, including Escape.
      if (document.querySelector('[role="dialog"], [role="alertdialog"]')) { reset(); return }
      const key = event.key.toLowerCase()
      if ((event.ctrlKey || event.metaKey) && !event.altKey && !event.shiftKey && key === 'k') { reset(); event.preventDefault(); actions.openCommand(); return }
      if (event.ctrlKey || event.metaKey || event.altKey) { reset(); return }
      const now = (actions.now || Date.now)()
      if (prefixAt !== undefined && now - prefixAt < 1200 && (key === 'i' || key === 'w')) {
        reset(); event.preventDefault(); actions.navigate(key === 'i' ? '/mail' : '/work'); return
      }
      reset()
      if (key === 'g') { prefixAt = now; return }
      if (key === 'c') { event.preventDefault(); actions.navigate('/compose'); return }
      if (key === '/') { event.preventDefault(); actions.openCommand(); return }
      if (key === '?') { event.preventDefault(); actions.showHelp(); return }
      if (key === 'escape') { if (actions.closePanels() || actions.mail('close')) event.preventDefault(); return }
      const mailKeys: Record<string, MailCommand> = { r: 'reply', a: 'reply_all', f: 'forward', e: 'archive', j: 'next', k: 'previous' }
      if (mailKeys[key] && actions.mail(mailKeys[key])) event.preventDefault()
    },
  }
}
