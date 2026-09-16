import {useState} from 'react'
import {cleanup, fireEvent, render, screen} from '@testing-library/react'
import {afterEach, expect, it, vi} from 'vitest'
import {Link, MemoryRouter, Route, Routes, useLocation} from 'react-router-dom'
import {PageErrorBoundary, WorkspaceErrorPage} from './PageErrorBoundary'

afterEach(() => {cleanup(); vi.restoreAllMocks()})
function Broken(): never {throw new Error('private-mail-body-and-secret-stack')}
function Shell() {
  const location = useLocation()
  return <><nav><Link to="/safe">안전한 메뉴</Link></nav><PageErrorBoundary resetKey={location.key}><Routes><Route path="/bad" element={<Broken/>}/><Route path="/safe" element={<h1>정상 화면</h1>}/></Routes></PageErrorBoundary></>
}
it('contains a page crash, hides its details and recovers on navigation', async () => {
  vi.spyOn(console, 'error').mockImplementation(() => {})
  render(<MemoryRouter initialEntries={['/bad']}><Shell/></MemoryRouter>)
  expect(screen.getByRole('alert')).toHaveTextContent('이 화면을 표시하지 못했습니다')
  expect(document.body).not.toHaveTextContent('private-mail-body')
  fireEvent.click(screen.getByRole('link', {name: '안전한 메뉴'}))
  await screen.findByRole('heading', {name: '정상 화면'})
  expect(screen.queryByRole('alert')).not.toBeInTheDocument()
})
it('does not remount healthy content on a location change', () => {
  const mount = vi.fn()
  function Child() {const [value] = useState(() => {mount(); return '편집 내용'}); return <p>{value}</p>}
  const view = render(<MemoryRouter><PageErrorBoundary resetKey="one"><Child/></PageErrorBoundary></MemoryRouter>)
  view.rerender(<MemoryRouter><PageErrorBoundary resetKey="two"><Child/></PageErrorBoundary></MemoryRouter>)
  expect(mount).toHaveBeenCalledTimes(1)
})
it('offers a safe root fallback without exposing router errors', () => {
  render(<WorkspaceErrorPage/>)
  expect(screen.getByRole('link', {name: '워크스페이스 다시 열기'})).toHaveAttribute('href', '/app/mail')
})
