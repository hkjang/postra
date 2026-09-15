import {afterEach, beforeEach, expect, it, vi} from 'vitest'
import {cleanup, render, screen, waitFor} from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import {MemoryRouter} from 'react-router-dom'
import {QueryClient, QueryClientProvider} from '@tanstack/react-query'
import {api} from '@/api/client'
import {AskWorkspace} from './AskWorkspace'

vi.mock('@/api/client',()=>({api:vi.fn()}))
const mockAPI = vi.mocked(api)
beforeEach(()=>{mockAPI.mockReset();mockAPI.mockImplementation(async(path)=>{
  if(path==='/api/accounts')return [{id:'my-account',email:'me@corp.local'}] as never
  if(path==='/api/preferences')return {fields:[],revision:'r1'} as never
  return {model:'local-model',result_json:JSON.stringify({answer:'견적 검토가 진행 중입니다.',evidence_message_ids:['own-message']}),retrieval:{mode:'work',time_zone:'Asia/Seoul',sources:[{message_id:'own-message',subject:'견적 검토',from:'sender@corp.local',date:1,work_status:'in_progress',action_ids:['own-action']}],warnings:['일부 업무를 참고했습니다.'],bounded:true}} as never
})})
afterEach(cleanup)
function show(){render(<QueryClientProvider client={new QueryClient({defaultOptions:{queries:{retry:false}}})}><MemoryRouter><AskWorkspace/></MemoryRouter></QueryClientProvider>)}

it('sends shared retrieval filters and displays real source titles and limitations',async()=>{
  const user=userEvent.setup();show()
  await screen.findByRole('option',{name:'me@corp.local'})
  await user.type(screen.getByRole('textbox',{name:'AI에게 질문'}),'미완료 업무 정리')
  await user.selectOptions(screen.getByLabelText('대상 계정'),'my-account')
  await user.selectOptions(screen.getByLabelText('검색 방식'),'work')
  await user.click(screen.getByLabelText('완료된 업무 제외'))
  await user.click(screen.getByText('검색 범위 지정'))
  await user.type(screen.getByRole('textbox',{name:'검색어'}),'견적')
  await user.click(screen.getByRole('button',{name:'질문하기'}))
  expect(await screen.findByText('견적 검토가 진행 중입니다.')).toBeInTheDocument()
  expect(screen.getByRole('note')).toHaveTextContent('일부 업무')
  expect(screen.getByRole('link',{name:/견적 검토/})).toHaveAttribute('href','/messages/own-message')
  expect(mockAPI).toHaveBeenCalledWith('/api/qa',expect.objectContaining({body:expect.objectContaining({account_id:'my-account',mode:'work',search_text:'견적',incomplete_only:true})}))
})

it('shows a policy-locked default search mode',async()=>{
  mockAPI.mockImplementation(async(path)=>path==='/api/preferences'?{fields:[{key:'search.default_mode',value:'keyword',locked:true}],revision:'r2'} as never:[] as never)
  show()
  await waitFor(()=>expect(screen.getByLabelText('검색 방식')).toBeDisabled())
  expect(screen.getByLabelText('검색 방식')).toHaveValue('keyword')
})
