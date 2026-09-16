import {afterEach,expect,it,vi} from 'vitest'
import {cleanup,render,screen} from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import {ReceivedImages} from './ReceivedImages'
import {mailDocument,MailBody} from './MailBody'

afterEach(()=>{cleanup();vi.restoreAllMocks()})
const body={text_body:'본문',external_images:2,image_policy:'allow_domain',images_allowed:false}

it('blocks all remote resources by default and only relaxes img-src after server authorization',()=>{
  expect(mailDocument('<p>본문</p>')).toContain("img-src 'none'")
  const allowed=mailDocument('<img src="https://images.test/photo.png">',true)
  expect(allowed).toContain('img-src https: http:')
  expect(allowed).toContain("default-src 'none'")
  expect(allowed).toContain('<meta name="referrer" content="no-referrer">')
  render(<MailBody html="<b>본문</b>" imagesAllowed/>)
  expect(screen.getByTitle('메일 본문')).toHaveAttribute('sandbox','')
  expect(screen.getByTitle('메일 본문')).toHaveAttribute('referrerpolicy','no-referrer')
})
it('does not show allow controls when admin policy blocks images',()=>{
  const once=vi.fn(),trust=vi.fn()
  render(<ReceivedImages body={{...body,image_policy:'block'}} busy={false} once={once} trust={trust}/>)
  expect(screen.getByText(/관리자 정책에 따라/)).toBeInTheDocument()
  expect(screen.queryByRole('button')).toBeNull()
  expect(once).not.toHaveBeenCalled();expect(trust).not.toHaveBeenCalled()
})
it('uses an independently sandboxed same-origin document for authorized received images',()=>{
  render(<MailBody html="<p>본문</p>" receivedMessageID="own/message" allowImagesOnce/>)
  expect(screen.getByTitle('메일 본문')).toHaveAttribute('src','/api/messages/own%2Fmessage/body/frame?external_images=once')
  expect(screen.getByTitle('메일 본문')).not.toHaveAttribute('srcdoc')
  expect(screen.getByTitle('메일 본문')).toHaveAttribute('sandbox','allow-popups allow-popups-to-escape-sandbox')
})
it('separates one-time display from confirmed persistent trust and permits revocation',async()=>{
  const user=userEvent.setup(),once=vi.fn(),trust=vi.fn(),confirm=vi.spyOn(window,'confirm').mockReturnValue(false)
  const {rerender}=render(<ReceivedImages body={body} busy={false} once={once} trust={trust}/>)
  await user.click(screen.getByRole('button',{name:'이번만 이미지 표시'}));expect(once).toHaveBeenCalledTimes(1);expect(trust).not.toHaveBeenCalled()
  await user.click(screen.getByRole('button',{name:'이 도메인 이미지 항상 허용'}));expect(confirm).toHaveBeenCalled();expect(trust).not.toHaveBeenCalled()
  confirm.mockReturnValue(true);await user.click(screen.getByRole('button',{name:'이 발신자 이미지 항상 허용'}));expect(trust).toHaveBeenCalledWith('sender',false)
  rerender(<ReceivedImages body={{...body,images_allowed:true,image_domain_trusted:true}} busy={false} once={once} trust={trust}/>)
  expect(screen.queryByRole('button',{name:'이번만 이미지 표시'})).toBeNull()
  await user.click(screen.getByRole('button',{name:'도메인 이미지 허용 취소'}));expect(trust).toHaveBeenCalledWith('domain',true)
})
