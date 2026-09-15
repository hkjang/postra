import {afterEach, beforeEach, describe, expect, it, vi} from 'vitest'
import {cleanup, render, screen, waitFor} from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import {QueryClient, QueryClientProvider} from '@tanstack/react-query'
import {createMemoryRouter, Link, MemoryRouter, RouterProvider, useSearchParams} from 'react-router-dom'
import {api} from '@/api/client'
import {SettingsEditor} from './SettingsEditor'
import {AccountPreferencesPage} from './index'
import type {SettingField, SettingsView} from './preferences'

vi.mock('@/api/client', () => ({api:vi.fn()}))
const mockAPI=vi.mocked(api)
const field=(value:Partial<SettingField>):SettingField=>({key:'ui.theme',label:'테마',category:'appearance',type:'enum',value:'system',default:'system',options:['system','light','dark'],scope:'user',apply:'live',secret:false,source:'default',locked:false,lockable:true,...value})
let view:SettingsView
beforeEach(()=>{view={revision:'r1',fields:[field({}),field({key:'ai.api_key_ref',label:'AI API Key',category:'ai',type:'secret',scope:'admin',secret:true,registered:true,environment:['POSTRA_AI_API_KEY_REF'],value:''}),field({key:'ai.base_url',label:'AI Base URL',category:'ai',type:'url',scope:'admin',value:'https://ai.local/v1',environment:['POSTRA_AI_BASE_URL']})]};mockAPI.mockReset();mockAPI.mockImplementation(async(path,options)=>{if(path==='/api/accounts'||path==='/api/signatures')return [] as never;if(options?.method==='PATCH')return {...view,revision:'r2'} as never;return view as never})})
afterEach(cleanup)
function show(admin=false){return render(<QueryClientProvider client={new QueryClient({defaultOptions:{queries:{retry:false}}})}><MemoryRouter><SettingsEditor admin={admin}/></MemoryRouter></QueryClientProvider>)}

describe('layered settings editor',()=>{
  it('searches environment names across categories and hides registered secrets',async()=>{
    const user=userEvent.setup();show(true)
    expect(await screen.findByLabelText('AI API Key')).toHaveValue('')
    expect(screen.getByLabelText('AI API Key')).toHaveAttribute('type','password')
    await user.type(screen.getByLabelText('설정 검색'),'POSTRA_AI_BASE_URL')
    expect(screen.getByLabelText('AI Base URL')).toBeInTheDocument()
    expect(screen.queryByLabelText('테마')).not.toBeInTheDocument()
  })
  it('requires change review before saving and clears newly entered keys on failure',async()=>{
    const user=userEvent.setup();show(true)
    const key=await screen.findByLabelText('AI API Key')
    await user.type(key,'never-display-this-key')
    expect(mockAPI.mock.calls.some(([,options])=>options?.method==='PATCH')).toBe(false)
    await user.click(screen.getByRole('button',{name:'변경 확인'}))
    expect(screen.getByRole('dialog')).not.toHaveTextContent('never-display-this-key')
    mockAPI.mockRejectedValueOnce(new Error('안전한 저장 실패 메시지'))
    await user.click(screen.getByRole('button',{name:'확인 후 저장'}))
    await waitFor(()=>expect(key).toHaveValue(''))
    const [,options]=mockAPI.mock.calls.find(([,options])=>options?.method==='PATCH')!
    expect(options?.body).toMatchObject({secrets:{'ai.api_key_ref':'never-display-this-key'},revision:'r1'})
  })
  it('disables administrator-forced fields for users',async()=>{
    view={revision:'locked',fields:[field({locked:true,source:'admin_policy',value:'dark'})]};show()
    expect(await screen.findByLabelText('테마')).toBeDisabled()
    expect(screen.getByText('조직 정책')).toBeInTheDocument()
    expect(screen.queryByText('기본값 상속으로 복원')).not.toBeInTheDocument()
  })
  it('shows fixed approval policy as always-on and read-only even for administrators',async()=>{
    view={revision:'fixed',fields:[field({key:'send.external_approval',label:'외부 메일 승인 필수',category:'send',type:'bool',scope:'admin',apply:'fixed',locked:true,lockable:false,source:'fixed_policy',value:'true',default:'true'})]};show(true)
    const control=await screen.findByLabelText('외부 메일 승인 필수')
    expect(control).toBeDisabled()
    expect(control).toBeChecked()
    expect(screen.getByText('항상 적용')).toBeInTheDocument()
    expect(screen.queryByText('배포 초기값')).not.toBeInTheDocument()
    expect(screen.queryByText('관리자 값 강제 적용')).not.toBeInTheDocument()
    expect(screen.getByRole('button',{name:'변경 확인'})).toBeDisabled()
  })
  it('guards SPA navigation without showing the unsaved credential',async()=>{
    const user=userEvent.setup()
    const router=createMemoryRouter([{path:'/',element:<><Link to="/away">다른 화면</Link><SettingsEditor admin/></>},{path:'/away',element:<h1>이동 완료</h1>}])
    render(<QueryClientProvider client={new QueryClient({defaultOptions:{queries:{retry:false}}})}><RouterProvider router={router}/></QueryClientProvider>)
    await user.type(await screen.findByLabelText('AI API Key'),'unsaved-key-keep-private')
    await user.click(screen.getByRole('link',{name:'다른 화면'}))
    expect(await screen.findByRole('dialog')).not.toHaveTextContent('unsaved-key-keep-private')
    await user.click(screen.getByRole('button',{name:'계속 편집'}))
    expect(screen.getByLabelText('AI API Key')).toHaveValue('unsaved-key-keep-private')
    await user.click(screen.getByRole('link',{name:'다른 화면'}))
    await user.click(screen.getByRole('button',{name:'변경 버리고 이동'}))
    expect(await screen.findByRole('heading',{name:'이동 완료'})).toBeInTheDocument()
    expect(screen.queryByLabelText('AI API Key')).not.toBeInTheDocument()
  })
  it('discards pending values and revision when navigating between account preferences',async()=>{
    const user=userEvent.setup()
    // Two accounts may have exactly the same effective settings and revision.
    // Discarding changes while switching must not carry account A's form to B.
    view={revision:'same-effective-settings',fields:[field({key:'compose.tone',label:'AI 작성 톤',category:'compose',value:'professional',default:'professional',options:['professional','formal','friendly']})]}
    const router=createMemoryRouter([{path:'/accounts/:id/preferences',element:<><Link to="/accounts/account-b/preferences">B 계정 설정</Link><AccountPreferencesPage/></>}],{initialEntries:['/accounts/account-a/preferences']})
    render(<QueryClientProvider client={new QueryClient({defaultOptions:{queries:{retry:false}}})}><RouterProvider router={router}/></QueryClientProvider>)
    await user.selectOptions(await screen.findByLabelText('AI 작성 톤'),'formal')
    await user.click(screen.getByRole('link',{name:'B 계정 설정'}))
    await user.click(await screen.findByRole('button',{name:'변경 버리고 이동'}))
    expect(await screen.findByLabelText('AI 작성 톤')).toHaveValue('professional')
    expect(screen.getByRole('button',{name:'변경 확인'})).toBeDisabled()
    expect(mockAPI.mock.calls.some(([,options])=>options?.method==='PATCH')).toBe(false)
    await user.selectOptions(screen.getByLabelText('AI 작성 톤'),'friendly')
    await user.click(screen.getByRole('button',{name:'변경 확인'}))
    await user.click(screen.getByRole('button',{name:'확인 후 저장'}))
    await waitFor(()=>expect(mockAPI).toHaveBeenCalledWith('/api/accounts/account-b/preferences',expect.objectContaining({method:'PATCH',body:expect.objectContaining({values:{'compose.tone':'friendly'}})})))
  })
  it('clears discarded secrets when a category change keeps the admin editor mounted',async()=>{
    const user=userEvent.setup()
    view.fields=[...(view.fields || []),field({key:'send.max_per_minute',label:'분당 발송 제한',category:'send',type:'int',scope:'admin',value:'5'})]
    function RetainedAdminEditor() {
      const [params]=useSearchParams()
      return <><Link to="/?category=send">발송 정책 화면</Link><SettingsEditor admin category={params.get('category') || 'ai'}/></>
    }
    const router=createMemoryRouter([{path:'/',element:<RetainedAdminEditor/>}])
    render(<QueryClientProvider client={new QueryClient({defaultOptions:{queries:{retry:false}}})}><RouterProvider router={router}/></QueryClientProvider>)
    await user.type(await screen.findByLabelText('AI API Key'),'discarded-credential')
    await user.click(screen.getByRole('link',{name:'발송 정책 화면'}))
    await user.click(await screen.findByRole('button',{name:'변경 버리고 이동'}))
    const limit=await screen.findByLabelText('분당 발송 제한')
    expect(screen.getByRole('button',{name:'변경 확인'})).toBeDisabled()
    await user.clear(limit);await user.type(limit,'6')
    await user.click(screen.getByRole('button',{name:'변경 확인'}))
    await user.click(screen.getByRole('button',{name:'확인 후 저장'}))
    await waitFor(()=>expect(mockAPI).toHaveBeenCalledWith('/api/admin/configuration',expect.objectContaining({method:'PATCH',body:expect.objectContaining({values:{'send.max_per_minute':'6'},secrets:{}})})))
  })
})
