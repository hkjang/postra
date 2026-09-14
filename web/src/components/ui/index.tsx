import * as React from 'react'
import {Slot} from '@radix-ui/react-slot'
import {cva, type VariantProps} from 'class-variance-authority'
import {AlertCircle, Inbox, LoaderCircle} from 'lucide-react'
import {cn} from '@/lib/utils'

const buttonVariants = cva('button', {variants: {
  variant: {default: 'button-primary', secondary: 'button-secondary', ghost: 'button-ghost', destructive: 'button-danger', outline: 'button-outline'},
  size: {default: '', sm: 'button-sm', icon: 'button-icon'},
}, defaultVariants: {variant: 'default', size: 'default'}})
export const Button = React.forwardRef<HTMLButtonElement, React.ButtonHTMLAttributes<HTMLButtonElement> & VariantProps<typeof buttonVariants> & {asChild?: boolean}>(
  ({className, variant, size, asChild = false, type = 'button', ...props}, ref) => {
    const Component = asChild ? Slot : 'button'
    return <Component className={cn(buttonVariants({variant, size}), className)} ref={ref} type={asChild ? undefined : type} {...props}/>
  })
Button.displayName = 'Button'
export const Input = React.forwardRef<HTMLInputElement, React.InputHTMLAttributes<HTMLInputElement>>(({className, ...props}, ref) => <input ref={ref} className={cn('input', className)} {...props}/> )
Input.displayName = 'Input'
export const Textarea = React.forwardRef<HTMLTextAreaElement, React.TextareaHTMLAttributes<HTMLTextAreaElement>>(({className, ...props}, ref) => <textarea ref={ref} className={cn('input textarea', className)} {...props}/> )
Textarea.displayName = 'Textarea'
export function Badge({variant = 'default', className, ...props}: React.HTMLAttributes<HTMLSpanElement> & {variant?: 'default'|'secondary'|'outline'|'destructive'}) {return <span className={cn('badge', `badge-${variant}`, className)} {...props}/>}
export function Panel({className, ...props}: React.HTMLAttributes<HTMLElement>) {return <section className={cn('panel', className)} {...props}/>}
export function PageHeader({title, description, actions}: {title: string; description?: string; actions?: React.ReactNode}) {return <header className="page-header"><div><h1>{title}</h1>{description && <p className="muted">{description}</p>}</div>{actions && <div className="row">{actions}</div>}</header>}
export function EmptyState({title, description, action}: {title: string; description?: string; action?: React.ReactNode}) {return <div className="empty-state"><Inbox size={30}/><h2>{title}</h2>{description && <p className="muted">{description}</p>}{action}</div>}
export function ErrorState({error, retry}: {error: unknown; retry?: () => void}) {return <div className="error-state" role="alert"><AlertCircle size={18}/><span>{error instanceof Error ? error.message : '요청을 처리하지 못했습니다.'}</span>{retry && <Button variant="outline" size="sm" onClick={retry}>다시 시도</Button>}</div>}
export function Loading({label = '불러오는 중…'}: {label?: string}) {return <div className="loading" role="status"><LoaderCircle className="spin" size={20}/>{label}</div>}
