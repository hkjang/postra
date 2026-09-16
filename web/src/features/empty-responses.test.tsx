import {afterEach,beforeEach,describe,expect,it,vi} from 'vitest'
import {cleanup,render,screen} from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import {QueryClient,QueryClientProvider} from '@tanstack/react-query'
import {MemoryRouter,Route,Routes} from 'react-router-dom'
import {api} from '@/api/client'
import {SessionContext} from '@/app/session'
import {AccountsPage} from './accounts'
import {UsersPanel} from './admin/users'
import {AuditPanel,IncidentsPanel} from './admin/activity'
import {JobsPage} from './jobs'
import {SentPage} from './sent'
import {RulesPage} from './rules'
import {TeamPage,WorkPage} from './work'
import {AskPage,DigestPage} from './ai'
import {SettingsEditor} from './settings/SettingsEditor'

vi.mock('@/api/client',()=>({api:vi.fn()}))
const mockAPI=vi.mocked(api)
const principal={user_id:'empty-user',login_id:'admin',display_name:'관리자',role:'admin',auth_method:'local'}
beforeEach(()=>mockAPI.mockReset())
afterEach(cleanup)
function mount(node:React.ReactNode,path='/') {
  render(<QueryClientProvider client={new QueryClient({defaultOptions:{queries:{retry:false},mutations:{retry:false}}})}><SessionContext.Provider value={principal}><MemoryRouter initialEntries={[path]}><Routes><Route path="*" element={node}/><Route path="/jobs/:id" element={<JobsPage/>}/></Routes></MemoryRouter></SessionContext.Provider></QueryClientProvider>)
}

describe('valid empty Go responses across workspace pages',()=>{
  it.each([
    ['accounts',<AccountsPage/>,'연결된 메일 계정이 없습니다'],
    ['users',<UsersPanel/>,'등록된 사용자가 없습니다'],
    ['audit',<AuditPanel/>,'감사 기록이 없습니다'],
    ['jobs',<JobsPage/>,'표시할 작업이 없습니다'],
    ['sent',<SentPage/>,'아직 발송한 메일이 없습니다'],
    ['rules',<RulesPage/>,'저장된 규칙이 없습니다'],
  ])('%s renders a null top-level list without a crash',async(_name,node,label)=>{
    mockAPI.mockResolvedValue(null);mount(node)
    expect(await screen.findByText(label as string)).toBeInTheDocument()
    expect(screen.queryByRole('alert')).not.toBeInTheDocument()
  })
  it('renders wrapped null rule lists',async()=>{
    mockAPI.mockResolvedValue({rules:null});mount(<RulesPage/>)
    expect(await screen.findByText('저장된 규칙이 없습니다')).toBeInTheDocument()
  })
  it('renders nullable work buckets and team items',async()=>{
    mockAPI.mockImplementation(async path=>path.startsWith('/api/work-inbox')?{important:null,snoozed_due:null,attention:null,reference:null,counts:{},generated_at:0}:{items:null})
    mount(<WorkPage/>)
    await userEvent.setup().click(screen.getByRole('button',{name:'메일 신호'}))
    expect(await screen.findAllByText('해당 업무가 없습니다')).toHaveLength(4)
    cleanup();mockAPI.mockResolvedValue({items:null});mount(<TeamPage/>)
    expect(await screen.findByText('등록된 처리 업무가 없습니다')).toBeInTheDocument()
  })
  it('renders nullable incident lists while retaining required statistics',async()=>{
    mockAPI.mockResolvedValue({incidents:null,stats:{open_total:0,open_critical:0,open_error:0,open_warning:0,resolved:0}})
    mount(<IncidentsPanel/>)
    expect(await screen.findByText('표시할 장애가 없습니다')).toBeInTheDocument()
  })
  it.each([['ask',<AskPage/>],['digest',<DigestPage/>],['settings',<SettingsEditor/>]])('%s tolerates no accounts/signatures and nullable setting fields',async(_name,node)=>{
    mockAPI.mockImplementation(async path=>path==='/api/preferences'?{revision:'empty',fields:null}:null)
    mount(node)
    if(_name==='settings')expect(await screen.findByText('검색된 설정이 없습니다')).toBeInTheDocument()
    else expect(await screen.findByRole('option',{name:'내 모든 계정'})).toBeInTheDocument()
  })
})

describe('malformed successful responses are query failures, not empty lists',()=>{
  it.each([
    ['jobs',<JobsPage/>,{}],['jobs null item',<JobsPage/>,[null]],
    ['sent',<SentPage/>,{}],['sent null item',<SentPage/>,[null]],['sent null outbound',<SentPage/>,[{outbound:null}]],
    ['rules',<RulesPage/>,{}],['rules bad list',<RulesPage/>,{rules:{}}],['rules null item',<RulesPage/>,{rules:[null]}],
  ])('%s reports a safe format error',async(_name,node,response)=>{
    mockAPI.mockResolvedValue(response);mount(node)
    expect(await screen.findByRole('alert')).toHaveTextContent('서버 응답 형식')
  })
  it('does not turn a null job detail into a successful empty list',async()=>{
    mockAPI.mockResolvedValue(null);mount(<JobsPage/>,'/jobs/job-missing')
    expect(await screen.findByRole('alert')).toHaveTextContent('서버 응답 형식')
    expect(screen.queryByText('표시할 작업이 없습니다')).not.toBeInTheDocument()
  })
  it('retains null job statistics as no statistics',async()=>{
    mockAPI.mockResolvedValue([{id:'job-1',type:'sync',status:'succeeded',created_at:0,stats:null}]);mount(<JobsPage/>)
    expect(await screen.findByRole('link',{name:'sync'})).toBeInTheDocument()
    expect(screen.queryByRole('alert')).not.toBeInTheDocument()
  })
})
