import { afterEach, expect, it, vi } from 'vitest'
import { act, cleanup, render, screen } from '@testing-library/react'
import { dispatchMailCommand, registerMailCommands, useAvailableMailCommands } from './mail-commands'

afterEach(cleanup)
function Available() { return <div>{useAvailableMailCommands().join(',')}</div> }
it('only exposes registered view actions and unregisters them without retaining private data', () => {
  const reply = vi.fn(), next = vi.fn()
  render(<Available />)
  expect(dispatchMailCommand('reply')).toBe(false)
  let disposeMessage: () => void = () => {}, disposeList: () => void = () => {}
  act(() => { disposeMessage = registerMailCommands({ reply }); disposeList = registerMailCommands({ next }) })
  expect(screen.getByText('reply,next')).toBeInTheDocument()
  expect(dispatchMailCommand('reply')).toBe(true); expect(reply).toHaveBeenCalledOnce()
  act(disposeMessage); expect(dispatchMailCommand('reply')).toBe(false)
  expect(dispatchMailCommand('next')).toBe(true); expect(next).toHaveBeenCalledOnce()
  act(disposeList); expect(dispatchMailCommand('next')).toBe(false)
})
