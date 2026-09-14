import React from 'react'
import ReactDOM from 'react-dom/client'
import {QueryClientProvider} from '@tanstack/react-query'
import {RouterProvider} from 'react-router-dom'
import {Toaster} from 'sonner'
import {router} from './app/router'
import {newQueryClient} from './app/providers'
import '@fontsource/inter/latin-400.css'
import '@fontsource/inter/latin-500.css'
import '@fontsource/inter/latin-600.css'
import 'pretendard/dist/web/variable/pretendardvariable-dynamic-subset.css'
import './styles.css'

const client = newQueryClient()
ReactDOM.createRoot(document.getElementById('root')!).render(<React.StrictMode><QueryClientProvider client={client}><RouterProvider router={router}/><Toaster richColors closeButton position="bottom-right"/></QueryClientProvider></React.StrictMode>)
