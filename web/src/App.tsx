import { QueryClient, QueryClientProvider } from "@tanstack/react-query"
import { Overview } from "./Overview"

const client = new QueryClient({
  defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
})
export default function App() {
  return (
    <QueryClientProvider client={client}>
      <Overview />
    </QueryClientProvider>
  )
}
