import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { cleanup, render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { MemoryRouter } from 'react-router-dom';
import { api } from '@/api/client';
import { SessionContext } from '@/app/session';
import { AdminPage } from './index';
import { AISettingsPanel } from './settings';
import { ProvisioningPanel, PurgePanel, UsersPanel } from './users';

vi.mock('@/api/client', () => ({ api: vi.fn() }));
vi.mock('sonner', () => ({ toast: { success: vi.fn(), error: vi.fn() } }));
const mockAPI = vi.mocked(api);
const principal = { user_id: 'usr_admin', login_id: 'admin', display_name: '관리자', role: 'admin', auth_method: 'local' };
const settings = { 'ai.base_url': 'https://ai.corp.local/v1', 'ai.model': 'internal-model', 'ai.api_key_ref': 'secret_reference_only', 'ai.extra_headers': '{"Authorization":"Bearer must-not-be-redisplayed"}' };
beforeEach(() => { mockAPI.mockReset(); });
afterEach(() => { cleanup(); vi.restoreAllMocks(); });

describe('admin identity and destructive operations', () => {
  it('does not fetch administration data for a non-admin user', () => {
    render(<MemoryRouter><SessionContext.Provider value={{ ...principal, role: 'user' }}><AdminPage /></SessionContext.Provider></MemoryRouter>);
    expect(screen.getByText('관리자 권한이 필요합니다')).toBeInTheDocument();
    expect(mockAPI).not.toHaveBeenCalled();
  });

  it('loads users without mutation and requires deletion confirmation', async () => {
    const user = userEvent.setup();
    mockAPI.mockResolvedValue([{ id: 'usr_one', login_id: 'hong', display_name: '홍길동', email: 'hong@corp.local', role: 'user', status: 'active', auth_provider: 'oidc' }]);
    const confirm = vi.spyOn(window, 'confirm').mockReturnValue(false);
    render(<SessionContext.Provider value={principal}><UsersPanel /></SessionContext.Provider>);
    await screen.findByText('홍길동');
    expect(mockAPI.mock.calls.every(([, options]) => !options?.method)).toBe(true);
    await user.click(screen.getByRole('button', { name: /^삭제$/ }));
    expect(confirm).toHaveBeenCalledWith(expect.stringContaining('hong@corp.local'));
    expect(mockAPI.mock.calls.some(([, options]) => options?.method === 'DELETE')).toBe(false);
  });

  it('requires matching emails and explicit confirmation before permanent purge', async () => {
    const user = userEvent.setup();
    const confirm = vi.spyOn(window, 'confirm').mockReturnValue(false);
    mockAPI.mockResolvedValue(undefined);
    render(<PurgePanel />);
    expect(mockAPI).not.toHaveBeenCalled();
    await user.type(screen.getByLabelText('이전 사용자 이메일'), 'hong@corp.local');
    await user.type(screen.getByLabelText('이메일 다시 입력'), 'other@corp.local');
    await user.click(screen.getByRole('button', { name: '이전 메일 영구 삭제' }));
    expect(await screen.findByRole('alert')).toHaveTextContent('일치하지 않습니다');
    expect(confirm).not.toHaveBeenCalled();
    await user.clear(screen.getByLabelText('이메일 다시 입력'));
    await user.type(screen.getByLabelText('이메일 다시 입력'), 'hong@corp.local');
    await user.click(screen.getByRole('button', { name: '이전 메일 영구 삭제' }));
    expect(mockAPI).not.toHaveBeenCalled();
    confirm.mockReturnValue(true);
    await user.click(screen.getByRole('button', { name: '이전 메일 영구 삭제' }));
    await waitFor(() => expect(mockAPI).toHaveBeenCalledWith('/api/admin/deleted-mail/purge', { method: 'POST', body: { email: 'hong@corp.local', confirmation: 'hong@corp.local' } }));
  });
});

describe('write-only credentials', () => {
  it('preserves the saved AI key by omission and never fills saved authentication headers', async () => {
    const user = userEvent.setup();
    mockAPI.mockResolvedValue(settings);
    render(<AISettingsPanel />);
    const key = await screen.findByLabelText(/^AI API Key/);
    expect(key).toHaveValue('');
    expect(key).toHaveAttribute('type', 'password');
    expect(screen.getByLabelText(/^추가 API 헤더/)).toHaveValue('');
    expect(screen.queryByDisplayValue('must-not-be-redisplayed')).not.toBeInTheDocument();
    await user.click(screen.getAllByRole('button', { name: '설정 저장' })[0]);
    await waitFor(() => expect(mockAPI.mock.calls.some(([, options]) => options?.method === 'PUT')).toBe(true));
    const [, options] = mockAPI.mock.calls.find(([, options]) => options?.method === 'PUT')!;
    expect(options?.body).not.toHaveProperty('api_key');
    expect(options?.body).not.toHaveProperty(['values', 'ai.api_key_ref']);
    expect(options?.body).not.toHaveProperty(['values', 'ai.extra_headers']);
  });

  it('clears a newly entered AI key after a failed save', async () => {
    const user = userEvent.setup();
    mockAPI.mockImplementation(async (_path, options) => { if (options?.method === 'PUT') throw new Error('연결 설정을 확인하세요.'); return settings as never; });
    render(<AISettingsPanel />);
    const key = await screen.findByLabelText(/^AI API Key/);
    await user.type(key, 'new-private-api-key');
    await user.click(screen.getAllByRole('button', { name: '설정 저장' })[0]);
    expect(await screen.findByRole('alert')).toHaveTextContent('연결 설정을 확인하세요.');
    expect(key).toHaveValue('');
    expect(screen.queryByText('new-private-api-key')).not.toBeInTheDocument();
  });
});

describe('email-only SSO mail provisioning', () => {
  it('stores a target-free common policy with shared password and unauthenticated SMTP', async () => {
    const user = userEvent.setup();
    vi.spyOn(window, 'confirm').mockReturnValue(true);
    mockAPI.mockResolvedValue({});
    render(<ProvisioningPanel />);
    expect(mockAPI).not.toHaveBeenCalled();
    expect(screen.queryByLabelText(/^대상 사용자 이메일/)).not.toBeInTheDocument();
    await user.type(screen.getByLabelText('IMAP 서버'), 'imap.corp.local');
    await user.type(screen.getByLabelText('SMTP 서버'), 'smtp.corp.local');
    const password = screen.getByLabelText(/^메일 비밀번호/);
    await user.type(password, 'shared-mail-secret');
    await user.click(screen.getByRole('button', { name: '프로비저닝 설정 저장' }));
    await waitFor(() => expect(mockAPI).toHaveBeenCalledWith('/api/admin/mail-accounts', expect.objectContaining({ method: 'POST' })));
    const body = mockAPI.mock.calls[0][1]?.body;
    expect(body).toMatchObject({ email: '', apply_to_all_users: true, auth_username: '', mail_password: 'shared-mail-secret', same_password: true, smtp_auth: 'none', smtp_password: '', automatic_sync: true });
    expect(body).not.toHaveProperty('user_id');
    expect(body).not.toHaveProperty('target_user');
    expect(password).toHaveValue('');
  });

  it('accepts a specific target only through an email input', async () => {
    const user = userEvent.setup();
    mockAPI.mockResolvedValue({});
    render(<ProvisioningPanel />);
    await user.click(screen.getByLabelText('특정 이메일 사용자에게 먼저 적용'));
    const email = screen.getByLabelText(/^대상 사용자 이메일/);
    expect(email).toHaveAttribute('type', 'email');
    await user.type(email, 'hong@corp.local');
    await user.type(screen.getByLabelText('IMAP 서버'), 'imap.corp.local');
    await user.type(screen.getByLabelText('SMTP 서버'), 'smtp.corp.local');
    await user.type(screen.getByLabelText(/^메일 비밀번호/), 'mail-secret');
    await user.click(screen.getByRole('button', { name: '프로비저닝 설정 저장' }));
    await waitFor(() => expect(mockAPI).toHaveBeenCalled());
    expect(mockAPI.mock.calls[0][1]?.body).toMatchObject({ email: 'hong@corp.local', apply_to_all_users: false });
  });
});
