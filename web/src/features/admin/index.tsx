import {useSession} from '@/app/session'
import {EmptyState} from '@/components/ui'
import {OperationsConsole} from './OperationsConsole'

export {MCPKeysPage} from '@/features/mcp'

export function AdminPage() {
  const principal = useSession()
  if (principal?.role !== 'admin') return <div className="page"><EmptyState title="관리자 권한이 필요합니다" description="메일은 자신의 계정에서만 확인할 수 있습니다."/></div>
  return <OperationsConsole/>
}
