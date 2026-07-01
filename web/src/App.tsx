import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { BrowserRouter, Routes, Route, NavLink } from 'react-router-dom'
import KeysPage from './pages/KeysPage'
import PricesPage from './pages/PricesPage'
import ConfigPage from './pages/ConfigPage'
import LoginPage from './pages/LoginPage'
import { logout } from './lib/api'

const queryClient = new QueryClient()

export default function App() {
  return (
    <QueryClientProvider client={queryClient}>
      <BrowserRouter>
        <Routes>
          <Route path="/login" element={<LoginPage />} />
          <Route path="*" element={<AdminLayout />} />
        </Routes>
      </BrowserRouter>
    </QueryClientProvider>
  )
}

function AdminLayout() {
  return (
    <div style={{ minHeight: '100vh', background: '#f9fafb' }}>
      <nav style={{ background: '#fff', borderBottom: '1px solid #e5e7eb', padding: '12px 24px', display: 'flex', gap: 24, fontSize: 14, fontWeight: 500, alignItems: 'center' }}>
        <span style={{ fontWeight: 700, marginRight: 16 }}>API Proxy</span>
        <NavLink to="/" end style={({ isActive }) => ({ color: isActive ? '#2563eb' : '#6b7280', textDecoration: 'none' })}>
          Keys
        </NavLink>
        <NavLink to="/prices" style={({ isActive }) => ({ color: isActive ? '#2563eb' : '#6b7280', textDecoration: 'none' })}>
          Prices
        </NavLink>
        <NavLink to="/config" style={({ isActive }) => ({ color: isActive ? '#2563eb' : '#6b7280', textDecoration: 'none' })}>
          Config
        </NavLink>
        <button
          onClick={() => logout()}
          style={{ marginLeft: 'auto', background: 'none', border: 'none', color: '#6b7280', cursor: 'pointer', fontSize: 13 }}
        >
          登出
        </button>
      </nav>
      <main style={{ maxWidth: 1100, margin: '0 auto', padding: 24 }}>
        <Routes>
          <Route path="/" element={<KeysPage />} />
          <Route path="/prices" element={<PricesPage />} />
          <Route path="/config" element={<ConfigPage />} />
        </Routes>
      </main>
    </div>
  )
}
