import { useState } from "react"
import { QueryClient, QueryClientProvider } from "@tanstack/react-query"
import { Unlock } from "./Unlock"
import { Overview } from "./Overview"

function sessionFor(token: string) {
  return {
    token,
    client: new QueryClient({
      defaultOptions: {
        queries: { retry: false },
        mutations: { retry: false },
      },
    }),
  }
}

export default function App() {
  const demo =
    document.querySelector('meta[name="agentd-isolated-demo"]') !== null
  const [session, setSession] = useState(() => (demo ? sessionFor("") : null))
  if (!session)
    return <Unlock unlock={(token) => setSession(sessionFor(token))} />
  return (
    <QueryClientProvider client={session.client}>
      <Overview
        token={session.token}
        lock={
          demo
            ? undefined
            : () => {
                session.client.clear()
                setSession(null)
              }
        }
      />
    </QueryClientProvider>
  )
}
