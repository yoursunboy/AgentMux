import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'

import { App } from './App'
import './styles/global.css'

const container = document.getElementById('root')
if (!container) {
  // Without the mount point nothing can render. Say so on the page rather
  // than failing silently in the console.
  throw new Error('AgentMux could not start: index.html has no #root element.')
}

createRoot(container).render(
  <StrictMode>
    <App />
  </StrictMode>,
)
