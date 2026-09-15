import {Link, useParams} from 'react-router-dom'
import {Button, PageHeader} from '@/components/ui'
import {SettingsEditor} from './SettingsEditor'

export function PersonalSettingsPage() {
  return <div className="page stack"><PageHeader title="개인 설정" description="내 화면, 메일 작성, AI와 알림을 설정합니다. 조직이 강제한 정책은 변경할 수 없습니다." actions={<Button asChild variant="outline"><Link to="/settings/signatures">서명 관리</Link></Button>}/><SettingsEditor/></div>
}
export function AccountPreferencesPage() {
  const {id} = useParams()
  return <div className="page stack"><PageHeader title="메일 계정 개인화" description="이 계정의 동기화·서명·HTML 템플릿·AI 스타일입니다. 비어 있는 선택값은 개인·조직 설정을 상속합니다." actions={<Button asChild variant="outline"><Link to={`/accounts/${id}`}>서버 연결 설정</Link></Button>}/><SettingsEditor key={id} accountID={id}/></div>
}
