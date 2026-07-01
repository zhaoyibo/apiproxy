import { useState } from 'react'
import { login } from '../lib/api'

export default function LoginPage() {
  const [key, setKey] = useState('')
  const [error, setError] = useState('')
  const [loading, setLoading] = useState(false)

  const handleSubmit = async (e: React.FormEvent) => {
    e.preventDefault()
    setError('')
    setLoading(true)
    try {
      await login(key)
      window.location.href = '/'
    } catch {
      setError('密钥错误')
    } finally {
      setLoading(false)
    }
  }

  return (
    <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'center', minHeight: '100vh', background: '#f9fafb' }}>
      <form onSubmit={handleSubmit} style={{ background: '#fff', padding: 32, borderRadius: 8, border: '1px solid #e5e7eb', width: 320 }}>
        <h2 style={{ margin: '0 0 20px', fontSize: 18 }}>API Proxy 管理</h2>
        <input
          type="password"
          placeholder="Admin Key"
          value={key}
          onChange={e => setKey(e.target.value)}
          style={{ width: '100%', padding: '8px 12px', border: '1px solid #d1d5db', borderRadius: 6, fontSize: 14, boxSizing: 'border-box', marginBottom: 12 }}
          autoFocus
        />
        {error && <p style={{ color: '#dc2626', fontSize: 13, margin: '0 0 10px' }}>{error}</p>}
        <button
          type="submit"
          disabled={loading}
          style={{ width: '100%', padding: '8px', background: '#2563eb', color: '#fff', border: 'none', borderRadius: 6, fontSize: 14, cursor: 'pointer' }}
        >
          {loading ? '登录中...' : '登录'}
        </button>
      </form>
    </div>
  )
}
