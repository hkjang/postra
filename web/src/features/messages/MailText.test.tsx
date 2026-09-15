import {afterEach,expect,it} from 'vitest'
import {cleanup,render,screen} from '@testing-library/react'
import {MailText} from './MailText'
afterEach(cleanup)
it('escapes untrusted mail while rendering safe links, quoted replies and simple emphasis',()=>{
  render(<MailText text={'Hello <script>alert(1)</script> & "world"\nVisit http://example.com/x?a=1&b=2 now.\nMail a.b@corp.local\n\n> quoted reply\n> second line\n**bold** and `code`'}/>)
  expect(document.querySelector('script')).toBeNull()
  expect(screen.getByText(/Hello <script>/)).toBeInTheDocument()
  expect(screen.getByRole('link',{name:'http://example.com/x?a=1&b=2'})).toHaveAttribute('href','http://example.com/x?a=1&b=2')
  expect(screen.getByRole('link',{name:'a.b@corp.local'})).toHaveAttribute('href','mailto:a.b@corp.local')
  expect(document.querySelector('blockquote')).toHaveTextContent('quoted replysecond line')
  expect(document.querySelector('strong')).toHaveTextContent('bold');expect(document.querySelector('code')).toHaveTextContent('code')
})
it('does not include punctuation in links or execute arbitrary schemes',()=>{
  render(<MailText text={'see (https://example.com). javascript:alert(1) data:text/html,bad https://name:password@example.com/'}/>)
  expect(screen.getByRole('link',{name:'https://example.com'})).toHaveAttribute('href','https://example.com/')
  expect(screen.getAllByRole('link')).toHaveLength(1)
  expect(screen.getByRole('link')).toHaveAttribute('rel','noopener noreferrer')
})
