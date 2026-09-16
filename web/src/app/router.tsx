import {createBrowserRouter} from 'react-router-dom'
import {App} from './App'
import {WorkspaceErrorPage} from '@/components/layout/PageErrorBoundary'
export const router = createBrowserRouter([{path: '*', element: <App/>, errorElement: <WorkspaceErrorPage/>}], {basename: '/app'})
