import { useSearchParams } from 'react-router-dom';
import { useSession } from '@/app/session';
import { Button, EmptyState, PageHeader } from '@/components/ui';
import { AISettingsPanel, GeneralSettingsPanel, OIDCSettingsPanel, SecuritySettingsPanel } from './settings';
import { PurgePanel, ProvisioningPanel, UsersPanel } from './users';
import { AuditPanel, IncidentsPanel, KeysPanel } from './activity';

export { MCPKeysPage } from './activity';

const tabs = [
  ['users', '사용자'], ['sso', 'SSO·메일 연결'], ['ai', 'AI'], ['security', '보안·발송'], ['settings', '동기화·정책'], ['incidents', '장애'], ['keys', 'MCP 키'], ['audit', '감사 기록'], ['data', '데이터 정리'],
] as const;

export function AdminPage() {
  const principal = useSession();
  const [search, setSearch] = useSearchParams();
  const requested = search.get('tab') || 'users';
  const tab = tabs.some(([key]) => key === requested) ? requested : 'users';
  if (principal?.role !== 'admin') return <div className="page"><EmptyState title="관리자 권한이 필요합니다" description="메일은 자신의 계정에서만 확인할 수 있습니다." /></div>;
  return <div className="page stack"><PageHeader title="관리자 설정" description="사용자, SSO, 조직의 메일·AI 운영 정책을 관리합니다." />
    <nav className="tabs row" aria-label="관리자 메뉴" style={{ flexWrap: 'wrap' }}>{tabs.map(([key, label]) => <Button key={key} className={tab === key ? 'active' : undefined} variant={tab === key ? 'default' : 'ghost'} size="sm" aria-current={tab === key ? 'page' : undefined} onClick={() => setSearch({ tab: key })}>{label}</Button>)}</nav>
    {tab === 'users' && <UsersPanel />}
    {tab === 'sso' && <><OIDCSettingsPanel /><ProvisioningPanel /></>}
    {tab === 'ai' && <AISettingsPanel />}
    {tab === 'security' && <SecuritySettingsPanel />}
    {tab === 'settings' && <GeneralSettingsPanel />}
    {tab === 'incidents' && <IncidentsPanel />}
    {tab === 'keys' && <><KeysPanel /><KeysPanel admin /></>}
    {tab === 'audit' && <AuditPanel />}
    {tab === 'data' && <PurgePanel />}
  </div>;
}
