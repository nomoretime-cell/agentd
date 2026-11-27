import { createContext, memo, useContext, type ReactNode } from "react"
import Markdown, { type Components } from "react-markdown"
import remarkGfm from "remark-gfm"
import "./reply-markdown.css"

const InsideLink = createContext(false)

function SourceLink({
  href,
  children,
}: {
  href?: string
  children: ReactNode
}) {
  // A Markdown image can itself be the label of a link.
  const nested = useContext(InsideLink)
  if (!href || nested) return <>{children}</>
  return (
    <InsideLink.Provider value={true}>
      <a href={href} target="_blank" rel="noopener noreferrer">
        {children}
      </a>
    </InsideLink.Provider>
  )
}

const components: Components = {
  a: ({ href, children }) => <SourceLink href={href}>{children}</SourceLink>,
  img: ({ src, alt }) => (
    <SourceLink href={typeof src === "string" ? src : undefined}>
      {alt ? `图片：${alt}` : "图片来源"}
    </SourceLink>
  ),
  table: ({ children }) => (
    <div className="reply-table">
      <table>{children}</table>
    </div>
  ),
}

export const ReplyMarkdown = memo(function ReplyMarkdown({
  text,
}: {
  text: string
}) {
  return (
    <div className="reply-markdown">
      <Markdown
        skipHtml
        remarkPlugins={[remarkGfm]}
        components={components}
        urlTransform={(url) => (/^https?:\/\//i.test(url) ? url : undefined)}
      >
        {text}
      </Markdown>
    </div>
  )
})
