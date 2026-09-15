import {afterEach,beforeEach,expect,it,vi} from 'vitest'
import {cleanup,render,screen} from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import {QueryClient,QueryClientProvider} from '@tanstack/react-query'
import {api} from '@/api/client'
import {AdvancedSearch,BulkActions,keywordSearchParams} from './SearchTools'
vi.mock('@/api/client',()=>({api:vi.fn()}))
const mockAPI=vi.mocked(api)
beforeEach(()=>mockAPI.mockReset()); afterEach(()=>{cleanup();vi.restoreAllMocks()})
function mount(element:React.ReactNode){render(<QueryClientProvider client={new QueryClient({defaultOptions:{queries:{retry:false}}})}>{element}</QueryClientProvider>)}
it('preserves account, detailed filters, snoozed folder and cursor across pages',()=>{
  const query=keywordSearchParams(new URLSearchParams('q=review&account=own&folder=snoozed&from=kim&to=hong&subject=contract&label=Legal&since=2026-09-01&until=2026-09-10&has_attachment=true'),'next')
  expect(Object.fromEntries(query)).toMatchObject({q:'review',account_id:'own',folder:'snoozed',from:'kim',to:'hong',subject:'contract',label:'Legal',has_attachment:'true',cursor:'next'})
  expect(Number(query.get('until'))).toBeGreaterThan(Number(query.get('since')))
  expect(keywordSearchParams(new URLSearchParams('since=invalid'),'').has('since')).toBe(false)
})
it('submits detailed field edits without a mutation or legacy form navigation',async()=>{
  const user=userEvent.setup(); const change=vi.fn(); mockAPI.mockResolvedValue([])
  mount(<AdvancedSearch params={new URLSearchParams()} onChange={change}/>); await user.click(screen.getByText('상세 검색 조건'))
  await user.type(screen.getByLabelText('보낸이'),'kim@corp.local'); await user.type(screen.getByLabelText('라벨'),'Legal')
  await user.click(screen.getByRole('button',{name:'조건 적용'}))
  expect(change).toHaveBeenCalledWith(expect.objectContaining({from:'kim@corp.local',label:'Legal'}))
  expect(mockAPI.mock.calls.every(([,options])=>!options?.body)).toBe(true)
})
it('uses only selected IDs and retains partial failures',async()=>{
  const user=userEvent.setup(); const success=vi.fn()
  mockAPI.mockResolvedValue({succeeded:1,failed:1,results:[{message_id:'m1',ok:true},{message_id:'m2',ok:false,error:'권한 확인 필요'}]})
  mount(<BulkActions ids={['m1','m2']} onSuccess={success}/>)
  await user.click(screen.getByRole('button',{name:'선택 메일에 적용'})); await screen.findByText('권한 확인 필요')
  expect(mockAPI).toHaveBeenCalledWith('/api/messages/batch',{body:{message_ids:['m1','m2'],action:'archive'}})
  expect(success).toHaveBeenCalledWith(['m1'])
})
it('cancels destructive bulk requests before calling the server',async()=>{
  const user=userEvent.setup();vi.spyOn(window,'confirm').mockReturnValue(false)
  mount(<BulkActions ids={['m1']} onSuccess={vi.fn()}/>);await user.selectOptions(screen.getByLabelText('선택 메일 작업'),'delete')
  await user.click(screen.getByRole('button',{name:'선택 메일에 적용'}));expect(mockAPI).not.toHaveBeenCalled()
})
