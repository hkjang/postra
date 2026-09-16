import {Component, type ReactNode} from 'react'
import {Link} from 'react-router-dom'
import {Button} from '@/components/ui'

// Keep navigation available if one page fails to render. Do not display error
// details (which may contain mail), reload automatically, or replay mutations.
export class PageErrorBoundary extends Component<{children: ReactNode; resetKey: string}, {failed: boolean}> {
  state = {failed: false}
  static getDerivedStateFromError() {return {failed: true}}
  componentDidUpdate(previous: Readonly<{children: ReactNode; resetKey: string}>) {
    if (this.state.failed && previous.resetKey !== this.props.resetKey) this.setState({failed: false})
  }
  render() {
    if (!this.state.failed) return this.props.children
    return <section className="page stack" role="alert"><h1>이 화면을 표시하지 못했습니다</h1><p>다시 시도하거나 다른 메뉴로 이동할 수 있습니다. 저장·발송 요청을 자동으로 반복하지 않습니다.</p><div className="row"><Button onClick={() => this.setState({failed: false})}>화면 다시 시도</Button><Button variant="outline" asChild><Link to="/mail">받은메일로</Link></Button></div></section>
  }
}

// The outer router also needs a safe fallback for shell/chunk-load failures.
export function WorkspaceErrorPage() {
  return <main className="auth-page"><section className="login-panel" role="alert"><h1>워크스페이스를 열지 못했습니다</h1><p>연결 상태를 확인한 후 다시 열어 주세요.</p><Button asChild><a href="/app/mail">워크스페이스 다시 열기</a></Button></section></main>
}
