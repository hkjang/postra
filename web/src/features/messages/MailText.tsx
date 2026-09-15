import {Fragment,type ReactNode} from 'react'

// Readable plain mail remains text, never an HTML injection surface. Only a
// small explicit inline grammar becomes React nodes; links need a user click.
function inline(text:string):ReactNode[] {
  const token=/\*\*[^*\n]+\*\*|`[^`\n]+`|https?:\/\/[^\s<>"']+|[a-z0-9.!#$%&'*+/=?^_`{|}~-]+@[a-z0-9.-]+\.[a-z]{2,}/gi
  const nodes:ReactNode[]=[];let from=0
  for(const match of text.matchAll(token)) {
    const index=match.index,value=match[0];nodes.push(text.slice(from,index));from=index+value.length
    if(value.startsWith('**')) nodes.push(<strong key={index}>{value.slice(2,-2)}</strong>)
    else if(value.startsWith('`')) nodes.push(<code key={index}>{value.slice(1,-1)}</code>)
    else {
      const link=value.replace(/[),.;!?:]+$/g,'');let href=''
      if(/^https?:\/\//i.test(link)){try{const url=new URL(link);if(!url.username&&!url.password)href=url.href}catch{ /* display malformed addresses as text */ }}
      else href='mailto:'+link
      nodes.push(href?<Fragment key={index}><a href={href} target="_blank" rel="noopener noreferrer">{link}</a>{value.slice(link.length)}</Fragment>:value)
    }
  }
  nodes.push(text.slice(from));return nodes
}
export function MailText({text}:{text:string}) {
  const groups:{quote:boolean;lines:string[]}[]=[]
  for(const line of text.split(/\r?\n/)) {
    const quote=/^\s*>/.test(line),value=quote?line.replace(/^\s*>\s?/,''):line
    const last=groups.at(-1);if(last&&last.quote===quote)last.lines.push(value);else groups.push({quote,lines:[value]})
  }
  return <div className="mail-body-text">{groups.map((group,index)=>{
    const content=group.lines.map((line,n)=><Fragment key={n}>{n>0&&<br/>}{inline(line)}</Fragment>)
    return group.quote?<blockquote key={index}>{content}</blockquote>:<div key={index}>{content}</div>
  })}</div>
}
