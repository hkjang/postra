import {afterEach,beforeEach,expect,it,vi} from 'vitest'
import {act,cleanup,render,screen} from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import {MemoryRouter,Route,Routes} from 'react-router-dom'
import {QueryClient,QueryClientProvider} from '@tanstack/react-query'
import {api} from '@/api/client'
import {MessagePage} from './MessagePage'
import {ThreadPage} from './ThreadPage'
import {AIContext} from './AIContext'
import {dispatchMailCommand} from '@/lib/mail-commands'
vi.mock('@/api/client',()=>({api:vi.fn()}))
const prefs=vi.hoisted(()=>({values:{} as Record<string,string>}))
vi.mock('@/features/settings/preferences',()=>({usePersonalPreferences:()=>({value:(key:string,fallback:string)=>prefs.values[key]??fallback,locked:()=>false})}))
const mockAPI=vi.mocked(api)
const message={id:'m1',thread_id:'t1',account_id:'own',subject:'프로젝트 회의',from:{email:'sender@corp.local'},date:1700000000,created_at:1700000000,has_attachments:true}
beforeEach(()=>{mockAPI.mockReset();prefs.values={}});afterEach(()=>{cleanup();vi.restoreAllMocks()})
function mount(path:string,element:React.ReactNode){render(<QueryClientProvider client={new QueryClient({defaultOptions:{queries:{retry:false}}})}><MemoryRouter initialEntries={[path]}><Routes><Route path="/messages/:id" element={element}/><Route path="/threads/:id" element={element}/></Routes></MemoryRouter></QueryClientProvider>)}
it('opens own conversation and distinguishes blocked from acknowledged quarantine downloads',async()=>{
  const user=userEvent.setup();const confirm=vi.spyOn(window,'confirm').mockReturnValue(false)
  mockAPI.mockResolvedValue({message,body:{text_body:'plain'},attachments:[{id:'clean',name:'정상.txt',scan_status:'clean',size:5},{id:'suspect',name:'주의.txt',scan_status:'quarantined',size:5},{id:'blocked',name:'차단.exe',scan_status:'blocked',size:5}]})
  mount('/messages/m1',<MessagePage/>);await screen.findByText('프로젝트 회의')
  expect(screen.getByRole('link',{name:'대화 전체 보기'})).toHaveAttribute('href','/threads/t1')
  expect(screen.getByRole('link',{name:'위험 확인 후 다운로드'})).toHaveAttribute('href','/api/messages/m1/attachments/suspect?ack=true')
  await user.click(screen.getByRole('link',{name:'위험 확인 후 다운로드'}));expect(confirm).toHaveBeenCalled()
  expect(document.querySelector('a[href*="attachments/blocked"]')).toBeNull()
  expect(document.querySelector('a[href^="/ui"]')).toBeNull()
})
it('renders the whole ordered thread and scoped single-message links',async()=>{
  mockAPI.mockResolvedValue({timeline:[{message,body:{text_body:'첫 메일'}},{message:{...message,id:'m2'},body:{text_body:'둘째 메일'}}],count:2})
  mount('/threads/t1',<ThreadPage/>);await screen.findByText('첫 메일');expect(screen.getByText('둘째 메일')).toBeInTheDocument()
  expect(mockAPI).toHaveBeenCalledWith('/api/threads/t1/timeline',expect.objectContaining({signal:expect.any(AbortSignal)}))
  expect(screen.getAllByRole('link',{name:'단독 보기'})).toHaveLength(2)
})
it('executes missing AI analyses only on explicit action and previews calendar extraction',async()=>{
  const user=userEvent.setup();mockAPI.mockResolvedValueOnce({result_json:'{"risk_level":"low","reason":"링크와 발신자 확인"}'}).mockResolvedValueOnce({events:[{title:'킥오프',start:'2026-09-20T10:00:00+09:00'}]})
  mount('/messages/m1',<AIContext message={message}/>);expect(mockAPI).not.toHaveBeenCalled()
  await user.selectOptions(screen.getByLabelText('상세 분석 종류'),'phishing');await user.click(screen.getByRole('button',{name:'선택한 분석 실행'}));await screen.findByText('링크와 발신자 확인')
  expect(mockAPI).toHaveBeenCalledWith('/api/messages/m1/analyze',{body:{type:'phishing'}})
  await user.click(screen.getByRole('button',{name:'일정 확인'}));await screen.findByText('킥오프')
  expect(screen.getByRole('link',{name:'일정 파일 (.ics) 다운로드'})).toHaveAttribute('href','/api/messages/m1/calendar.ics')
})
it('honors disabled AI replies and an opt-in automatic summary once on opening',async()=>{
  prefs.values={'ai.auto_summary':'true','ai.show_replies':'false'}
  mockAPI.mockResolvedValue({result_json:'{"summary":"선택한 자동 요약","requests":[],"dates":[]}'})
  mount('/messages/m1',<AIContext message={message}/>);await screen.findByText('선택한 자동 요약')
  expect(mockAPI).toHaveBeenCalledTimes(1)
  expect(screen.queryByRole('button',{name:'답장 제안'})).toBeNull()
})
it('marks a newly opened unread message once through a CSRF-protected API mutation',async()=>{
  mockAPI.mockResolvedValueOnce({message:{...message,is_read:false},body:{text_body:'읽기'}}).mockResolvedValueOnce({failed:0,results:[]}).mockResolvedValue({message:{...message,is_read:true},body:{text_body:'읽기'}})
  mount('/messages/m1',<MessagePage/>);await screen.findByText('안읽음 표시')
  expect(mockAPI.mock.calls.filter(([path])=>path==='/api/messages/batch')).toEqual([['/api/messages/batch',{method:'POST',body:{message_ids:['m1'],action:'mark_read'}}]])
})
it('registers actions only for the loaded owned message and prevents duplicate reply requests',async()=>{
  mockAPI.mockImplementation(async path=>{
    if(path==='/api/messages/m1')return {message:{...message,is_read:true},body:{text_body:'본문'}}
    if(path==='/api/drafts')return new Promise(()=>{})
    throw new Error('Unexpected path')
  })
  mount('/messages/m1',<MessagePage/>);expect(dispatchMailCommand('reply')).toBe(false)
  await screen.findByRole('heading',{name:'프로젝트 회의'})
  act(()=>{expect(dispatchMailCommand('reply')).toBe(true);dispatchMailCommand('reply')})
  await vi.waitFor(()=>expect(mockAPI.mock.calls.filter(([path])=>path==='/api/drafts')).toEqual([['/api/drafts',{method:'POST',body:{account_id:'own',kind:'reply',reply_to_message_id:'m1'}}]]))
  expect(mockAPI.mock.calls.some(([path])=>path.endsWith('/send')||path.endsWith('/request-approval'))).toBe(false)
  cleanup();expect(dispatchMailCommand('reply')).toBe(false);expect(dispatchMailCommand('copy_context')).toBe(false)
})
it('does not expose mail commands when the server rejects the requested message',async()=>{
  mockAPI.mockRejectedValue(new Error('내 메일만 조회할 수 있습니다.'))
  mount('/messages/m1',<MessagePage/>);await screen.findByText('내 메일만 조회할 수 있습니다.')
  for(const command of ['reply','reply_all','forward','archive','copy_context'] as const)expect(dispatchMailCommand(command)).toBe(false)
})
