import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import { cleanup, render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { MemoryRouter, Route, Routes } from 'react-router-dom';
import { api } from '@/api/client';
import { AccountDetailPage, AccountsPage } from './index';

vi.mock('@/api/client', () => ({ api: vi.fn() }));
vi.mock('sonner', () => ({ toast: { success: vi.fn(), error: vi.fn() } }));
const mockAPI = vi.mocked(api);
const account = { id: 'acc_self', name: '내 업무 메일', email: 'me@corp.local', status: 'active', inbound_protocol: 'imap', pop3_host: 'imap.corp.local', pop3_port: 993, pop3_security: 'tls', pop3_username: 'me@corp.local', pop3_secret_ref: 'existing-inbound', smtp_host: 'smtp.corp.local', smtp_port: 587, smtp_security: 'starttls', smtp_username: 'me@corp.local', smtp_auth: 'auto', smtp_secret_ref: 'existing-smtp', updated_at: 1234 };
beforeEach(() => mockAPI.mockReset());
afterEach(() => { cleanup(); vi.restoreAllMocks(); });

it('loads only current-user accounts and performs no mutation on render', async () => {
  mockAPI.mockResolvedValue([account]);
  render(<MemoryRouter><AccountsPage /></MemoryRouter>);
  await screen.findByText('내 업무 메일');
  expect(mockAPI).toHaveBeenCalledTimes(1);
  expect(mockAPI).toHaveBeenCalledWith('/api/accounts');
  expect(mockAPI.mock.calls.some(([path]) => path.startsWith('/api/admin'))).toBe(false);
});

it('preserves existing mail secrets by omission when saving ordinary settings', async () => {
  const user = userEvent.setup();
  mockAPI.mockResolvedValue(account);
  render(<MemoryRouter initialEntries={['/accounts/acc_self']}><Routes><Route path="/accounts/:id" element={<AccountDetailPage />} /></Routes></MemoryRouter>);
  const password = await screen.findByLabelText(/^새 수신 비밀번호/);
  expect(password).toHaveValue('');
  expect(screen.getByLabelText(/^새 SMTP 비밀번호/)).toHaveValue('');
  await user.click(screen.getByRole('button', { name: '변경 저장' }));
  await waitFor(() => expect(mockAPI.mock.calls.some(([, options]) => options?.method === 'PATCH')).toBe(true));
  const body = mockAPI.mock.calls.find(([, options]) => options?.method === 'PATCH')![1]?.body;
  expect(body).not.toHaveProperty('pop3_secret_ref');
  expect(body).not.toHaveProperty('smtp_secret_ref');
  expect(body).not.toHaveProperty('password');
  expect(body).not.toHaveProperty('smtp_password');
  expect(mockAPI.mock.calls.some(([path]) => path === '/api/secrets')).toBe(false);
});

it('clears SMTP credentials when changing to an unauthenticated internal relay', async () => {
  const user = userEvent.setup();
  mockAPI.mockResolvedValue(account);
  render(<MemoryRouter initialEntries={['/accounts/acc_self']}><Routes><Route path="/accounts/:id" element={<AccountDetailPage />} /></Routes></MemoryRouter>);
  await user.selectOptions(await screen.findByLabelText('SMTP 인증'), 'none');
  expect(screen.queryByLabelText(/^새 SMTP 비밀번호/)).not.toBeInTheDocument();
  await user.click(screen.getByRole('button', { name: '변경 저장' }));
  await waitFor(() => expect(mockAPI.mock.calls.some(([, options]) => options?.method === 'PATCH')).toBe(true));
  expect(mockAPI.mock.calls.find(([, options]) => options?.method === 'PATCH')![1]?.body).toMatchObject({ smtp_auth: 'none', smtp_username: '', smtp_secret_ref: '' });
});

it('does not delete the account when confirmation is cancelled', async () => {
  const user = userEvent.setup();
  const confirm = vi.spyOn(window, 'confirm').mockReturnValue(false);
  mockAPI.mockResolvedValue(account);
  render(<MemoryRouter initialEntries={['/accounts/acc_self']}><Routes><Route path="/accounts/:id" element={<AccountDetailPage />} /></Routes></MemoryRouter>);
  await user.click(await screen.findByRole('button', { name: '계정 연결 삭제' }));
  expect(confirm).toHaveBeenCalledWith(expect.stringContaining('me@corp.local'));
  expect(mockAPI.mock.calls.some(([, options]) => options?.method === 'DELETE')).toBe(false);
});
