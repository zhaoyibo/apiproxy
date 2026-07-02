import { useEffect, useMemo, useRef, useState } from 'react'
import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query'
import { Eye, EyeOff, Copy, Check, Pencil, X } from 'lucide-react'
import {
  listRootKeys, listChildKeys, createKey, updateKey, deleteKey, getKeyStats,
  type APIKey,
} from '../lib/api'

export default function KeysPage() {
  // Compute date range inside component so it refreshes if the page is left open overnight.
  const today = useMemo(() => new Date().toISOString().slice(0, 10), [])
  const sevenDaysAgo = useMemo(() => new Date(Date.now() - 7 * 86400_000).toISOString().slice(0, 10), [])
  const qc = useQueryClient()
  const [selectedRoot, setSelectedRoot] = useState<APIKey | null>(null)
  const [statsKeyId, setStatsKeyId] = useState<number | null>(null)
  const [showCreateRoot, setShowCreateRoot] = useState(false)
  const [showCreateChild, setShowCreateChild] = useState(false)

  const { data: rootKeys = [] } = useQuery({ queryKey: ['rootKeys'], queryFn: listRootKeys, refetchInterval: 30_000 })

  // Auto-select the first root key on initial load.
  const autoSelected = useRef(false)
  useEffect(() => {
    if (!autoSelected.current && rootKeys.length > 0) {
      autoSelected.current = true
      setSelectedRoot(rootKeys[0])
      setStatsKeyId(rootKeys[0].id)
    }
  }, [rootKeys])
  const { data: childKeys = [] } = useQuery({
    queryKey: ['childKeys', selectedRoot?.id],
    queryFn: () => listChildKeys(selectedRoot!.id),
    enabled: !!selectedRoot,
    refetchInterval: 30_000,
  })
  const { data: stats = [] } = useQuery({
    queryKey: ['stats', statsKeyId, sevenDaysAgo, today],
    queryFn: () => getKeyStats(statsKeyId!, sevenDaysAgo, today),
    enabled: !!statsKeyId,
    refetchInterval: 30_000,
  })

  const createMut = useMutation({
    mutationFn: createKey,
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['rootKeys'] })
      qc.invalidateQueries({ queryKey: ['childKeys', selectedRoot?.id] })
    },
  })
  const toggleMut = useMutation({
    mutationFn: ({ id, isActive }: { id: number; isActive: boolean }) =>
      updateKey(id, { is_active: isActive }),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['rootKeys'] })
      qc.invalidateQueries({ queryKey: ['childKeys', selectedRoot?.id] })
    },
  })
  const renameMut = useMutation({
    mutationFn: ({ id, name }: { id: number; name: string }) => updateKey(id, { name }),
    onSuccess: () => qc.invalidateQueries({ queryKey: ['childKeys', selectedRoot?.id] }),
  })
  const deleteMut = useMutation({
    mutationFn: (id: number) => deleteKey(id),
    onSuccess: (_data, deletedId) => {
      qc.invalidateQueries({ queryKey: ['rootKeys'] })
      qc.invalidateQueries({ queryKey: ['childKeys', selectedRoot?.id] })
      if (selectedRoot?.id === deletedId) setSelectedRoot(null)
    },
  })

  const [statsTab, setStatsTab] = useState<'all' | number>('all')

  const isRootStats = statsKeyId === selectedRoot?.id
  const childKeyMap = useMemo(() =>
    Object.fromEntries(childKeys.map(k => [k.id, k.name])),
    [childKeys]
  )

  // Reset tab to 'all' when switching stats target.
  useEffect(() => { setStatsTab('all') }, [statsKeyId])

  const visibleStats = useMemo(() =>
    isRootStats && statsTab !== 'all'
      ? stats.filter(s => s.key_id === statsTab)
      : stats,
    [stats, isRootStats, statsTab]
  )

  // Per-child totals for tab badges.
  const costByChild = useMemo(() => {
    const m: Record<number, number> = {}
    for (const s of stats) m[s.key_id] = (m[s.key_id] ?? 0) + parseFloat(s.cost_cny)
    return m
  }, [stats])

  const totalCost = visibleStats.reduce((s, r) => s + parseFloat(r.cost_cny), 0)

  return (
    <div style={{ display: 'flex', flexDirection: 'column', gap: 24 }}>
      {/* Top row: root keys + child keys */}
      <div style={{ display: 'grid', gridTemplateColumns: '280px 1fr', gap: 16, alignItems: 'start' }}>

        {/* Root keys */}
        <div>
          <SectionHeader title="主 Key" action={<Btn color="blue" onClick={() => setShowCreateRoot(true)}>+ 新建</Btn>} />
          <div style={{ display: 'flex', flexDirection: 'column', gap: 8 }}>
            {rootKeys.map(k => (
              <div
                key={k.id}
                onClick={() => { setSelectedRoot(k); setStatsKeyId(k.id) }}
                style={{
                  background: '#fff',
                  borderRadius: 10,
                  border: selectedRoot?.id === k.id ? '2px solid #2563eb' : '1px solid #e5e7eb',
                  padding: '12px 14px',
                  cursor: 'pointer',
                  transition: 'border-color 0.15s',
                }}
              >
                <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', marginBottom: 6 }}>
                  <span style={{ fontWeight: 600, fontSize: 14 }}>{k.name}</span>
                  <ActiveBadge active={k.is_active} />
                </div>
                <div style={{ marginBottom: 10 }} onClick={e => e.stopPropagation()}>
                  <KeyDisplay keyCode={k.key_code} />
                </div>
                <div style={{ display: 'flex', gap: 6 }} onClick={e => e.stopPropagation()}>
                  <Btn color="ghost" onClick={() => toggleMut.mutate({ id: k.id, isActive: !k.is_active })}>
                    {k.is_active ? '停用' : '启用'}
                  </Btn>
                  <Btn color="danger" onClick={() => deleteMut.mutate(k.id)}>删除</Btn>
                </div>
              </div>
            ))}
            {rootKeys.length === 0 && !showCreateRoot && (
              <p style={{ fontSize: 13, color: '#9ca3af', margin: 0 }}>暂无主 Key</p>
            )}
            {showCreateRoot && (
              <CreateKeyForm
                onSubmit={data => { createMut.mutate(data); setShowCreateRoot(false) }}
                onCancel={() => setShowCreateRoot(false)}
              />
            )}
          </div>
        </div>

        {/* Child keys */}
        <div>
          {selectedRoot ? (
            <>
              <SectionHeader
                title={<>子 Key <span style={{ color: '#6b7280', fontWeight: 400 }}>— {selectedRoot.name}</span></>}
                action={<Btn color="blue" onClick={() => setShowCreateChild(true)}>+ 新建子 Key</Btn>}
              />
              <div style={{ background: '#fff', borderRadius: 10, border: '1px solid #e5e7eb', overflow: 'hidden' }}>
                <table style={{ width: '100%', borderCollapse: 'collapse', fontSize: 13 }}>
                  <thead>
                    <tr style={{ background: '#f9fafb' }}>
                      {['名称', 'Key', '配额 (¥)', '已用 (¥)', '状态', '操作'].map(h => (
                        <th key={h} style={thStyle}>{h}</th>
                      ))}
                    </tr>
                  </thead>
                  <tbody>
                    {childKeys.map((k, i) => (
                      <tr key={k.id} style={{ borderTop: i === 0 ? 'none' : '1px solid #f3f4f6' }}>
                        <td style={tdStyle}>
                          <InlineEditName
                            value={k.name}
                            onSave={name => renameMut.mutate({ id: k.id, name })}
                          />
                        </td>
                        <td style={tdStyle}>
                          <KeyDisplay keyCode={k.key_code} />
                        </td>
                        <td style={tdStyle}>
                          {k.quota_cny === '-1' || k.quota_cny == null
                            ? <span style={{ color: '#9ca3af' }}>无限制</span>
                            : parseFloat(k.quota_cny).toFixed(2)}
                        </td>
                        <td style={tdStyle}>{parseFloat(k.used_cny).toFixed(6)}</td>
                        <td style={tdStyle}><ActiveBadge active={k.is_active} /></td>
                        <td style={{ ...tdStyle, whiteSpace: 'nowrap' }}>
                          <div style={{ display: 'flex', gap: 4 }}>
                            <Btn color="ghost" onClick={() => toggleMut.mutate({ id: k.id, isActive: !k.is_active })}>
                              {k.is_active ? '停用' : '启用'}
                            </Btn>
                            <Btn color="danger" onClick={() => deleteMut.mutate(k.id)}>删除</Btn>
                          </div>
                        </td>
                      </tr>
                    ))}
                    {childKeys.length === 0 && (
                      <tr>
                        <td colSpan={6} style={{ ...tdStyle, color: '#9ca3af', textAlign: 'center', padding: '20px 0' }}>
                          暂无子 Key
                        </td>
                      </tr>
                    )}
                  </tbody>
                </table>
              </div>
              {showCreateChild && (
                <div style={{ marginTop: 12 }}>
                  <CreateKeyForm
                    parentKey={selectedRoot.id}
                    onSubmit={data => { createMut.mutate(data); setShowCreateChild(false) }}
                    onCancel={() => setShowCreateChild(false)}
                  />
                </div>
              )}
            </>
          ) : (
            <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'center', height: 120, color: '#9ca3af', fontSize: 13 }}>
              ← 选择左侧主 Key 查看子 Key
            </div>
          )}
        </div>
      </div>

      {/* Stats */}
      {statsKeyId && stats.length > 0 && (
        <div>
          <SectionHeader
            title={
              <>
                用量统计（近7天）
                <span style={{ fontWeight: 400, fontSize: 13, color: '#6b7280', marginLeft: 8 }}>
                  共 ¥{totalCost.toFixed(6)}
                </span>
              </>
            }
          />
          {/* Child-key tabs (only shown for root key stats) */}
          {isRootStats && (
            <div style={{ display: 'flex', gap: 6, flexWrap: 'wrap', marginBottom: 10 }}>
              <button
                onClick={() => setStatsTab('all')}
                style={tabStyle(statsTab === 'all')}
              >
                全部
                <span style={tabBadgeStyle(statsTab === 'all')}>
                  ¥{stats.reduce((s, r) => s + parseFloat(r.cost_cny), 0).toFixed(4)}
                </span>
              </button>
              {Object.entries(costByChild).map(([kidStr, cost]) => {
                const kid = Number(kidStr)
                const name = childKeyMap[kid] ?? `#${kid}`
                return (
                  <button key={kid} onClick={() => setStatsTab(kid)} style={tabStyle(statsTab === kid)}>
                    {name}
                    <span style={tabBadgeStyle(statsTab === kid)}>¥{cost.toFixed(4)}</span>
                  </button>
                )
              })}
            </div>
          )}
          <div style={{ background: '#fff', borderRadius: 10, border: '1px solid #e5e7eb', overflow: 'hidden' }}>
            <table style={{ width: '100%', borderCollapse: 'collapse', fontSize: 13 }}>
              <thead>
                <tr style={{ background: '#f9fafb' }}>
                  {[...(isRootStats && statsTab === 'all' ? ['子 Key'] : []), '日期', '模型', '成功', '失败', '输入 Token', '输出 Token', '缓存命中', '缓存创建', '费用 (¥)'].map(h => (
                    <th key={h} style={thStyle}>{h}</th>
                  ))}
                </tr>
              </thead>
              <tbody>
                {visibleStats.map((s, i) => (
                  <tr key={s.id} style={{ borderTop: i === 0 ? 'none' : '1px solid #f3f4f6' }}>
                    {isRootStats && statsTab === 'all' && (
                      <td style={tdStyle}>
                        <span style={{ fontWeight: 500 }}>{childKeyMap[s.key_id] ?? `#${s.key_id}`}</span>
                      </td>
                    )}
                    <td style={tdStyle}>{s.date}</td>
                    <td style={tdStyle}><code style={{ fontSize: 12 }}>{s.model}</code></td>
                    <td style={tdStyle}>{s.call_count.toLocaleString()}</td>
                    <td style={{ ...tdStyle, color: s.fail_count > 0 ? '#ef4444' : undefined }}>
                      {s.fail_count.toLocaleString()}
                    </td>
                    <td style={tdStyle}>{s.input_tokens.toLocaleString()}</td>
                    <td style={tdStyle}>{s.output_tokens.toLocaleString()}</td>
                    <td style={tdStyle}>
                      {s.cache_hit_tokens.toLocaleString()}
                      {(() => {
                        const total = s.input_tokens + s.cache_hit_tokens + s.cache_write_tokens
                        if (total === 0) return null
                        const rate = (s.cache_hit_tokens / total * 100).toFixed(1)
                        return <span style={{ color: '#9ca3af', fontSize: 11, marginLeft: 4 }}>({rate}%)</span>
                      })()}
                    </td>
                    <td style={tdStyle}>{s.cache_write_tokens.toLocaleString()}</td>
                    <td style={{ ...tdStyle, fontWeight: 600 }}>{parseFloat(s.cost_cny).toFixed(6)}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </div>
      )}
    </div>
  )
}

function SectionHeader({ title, action }: { title: React.ReactNode; action?: React.ReactNode }) {
  return (
    <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', marginBottom: 10 }}>
      <h2 style={{ margin: 0, fontSize: 15, fontWeight: 600 }}>{title}</h2>
      {action}
    </div>
  )
}

function CreateKeyForm({ parentKey, onSubmit, onCancel }: {
  parentKey?: number
  onSubmit: (data: Partial<APIKey>) => void
  onCancel: () => void
}) {
  const [name, setName] = useState('')
  const [keyCode, setKeyCode] = useState('')
  const [quota, setQuota] = useState('')

  return (
    <form
      onSubmit={e => {
        e.preventDefault()
        onSubmit({
          name,
          key_code: keyCode || undefined,
          parent_id: parentKey,
          quota_cny: quota || undefined,
        })
      }}
      style={{ background: '#f9fafb', borderRadius: 10, border: '1px solid #e5e7eb', padding: 14 }}
    >
      <div style={{ display: 'flex', flexDirection: 'column', gap: 8 }}>
        <input required placeholder="名称" value={name} onChange={e => setName(e.target.value)} style={inputStyle} />
        <input placeholder="Key（留空自动生成）" value={keyCode} onChange={e => setKeyCode(e.target.value)} style={inputStyle} />
        <input placeholder="配额（元，留空无限制）" type="number" step="0.001" value={quota} onChange={e => setQuota(e.target.value)} style={inputStyle} />
        <div style={{ display: 'flex', gap: 8 }}>
          <Btn color="blue" type="submit">创建</Btn>
          <Btn color="ghost" type="button" onClick={onCancel}>取消</Btn>
        </div>
      </div>
    </form>
  )
}

function ActiveBadge({ active }: { active: boolean }) {
  return (
    <span style={{
      display: 'inline-block',
      fontSize: 11, padding: '2px 8px', borderRadius: 9999,
      background: active ? '#dcfce7' : '#f3f4f6',
      color: active ? '#16a34a' : '#9ca3af',
      fontWeight: 500,
    }}>
      {active ? '启用' : '停用'}
    </span>
  )
}

function Btn({ color, children, onClick, type = 'button' }: {
  color: 'blue' | 'danger' | 'ghost'
  children: React.ReactNode
  onClick?: () => void
  type?: 'button' | 'submit'
}) {
  const styles: Record<string, React.CSSProperties> = {
    blue:   { background: '#2563eb', color: '#fff', border: 'none' },
    danger: { background: 'transparent', color: '#ef4444', border: '1px solid #fecaca' },
    ghost:  { background: 'transparent', color: '#374151', border: '1px solid #e5e7eb' },
  }
  return (
    <button
      type={type}
      onClick={onClick}
      style={{
        ...styles[color],
        padding: '4px 12px', borderRadius: 6, fontSize: 12,
        cursor: 'pointer', whiteSpace: 'nowrap',
      }}
    >
      {children}
    </button>
  )
}

const thStyle: React.CSSProperties = {
  padding: '10px 14px', textAlign: 'left',
  fontSize: 12, fontWeight: 600, color: '#6b7280',
  borderBottom: '1px solid #e5e7eb',
}

const tdStyle: React.CSSProperties = {
  padding: '10px 14px', verticalAlign: 'middle',
}

const inputStyle: React.CSSProperties = {
  padding: '7px 10px', border: '1px solid #d1d5db',
  borderRadius: 6, fontSize: 13, width: '100%', boxSizing: 'border-box',
  background: '#fff',
}

function tabStyle(active: boolean): React.CSSProperties {
  return {
    display: 'flex', alignItems: 'center', gap: 6,
    padding: '5px 12px', borderRadius: 9999, fontSize: 12, cursor: 'pointer',
    border: active ? '1.5px solid #2563eb' : '1px solid #e5e7eb',
    background: active ? '#eff6ff' : '#fff',
    color: active ? '#2563eb' : '#374151',
    fontWeight: active ? 600 : 400,
  }
}

function tabBadgeStyle(active: boolean): React.CSSProperties {
  return {
    fontSize: 11,
    color: active ? '#2563eb' : '#9ca3af',
  }
}

function maskKey(key: string): string {
  if (key.length <= 8) return '••••••••'
  return key.slice(0, 4) + '••••••••' + key.slice(-4)
}

function KeyDisplay({ keyCode }: { keyCode: string }) {
  const [visible, setVisible] = useState(false)
  const [copied, setCopied] = useState(false)

  const handleCopy = (e: React.MouseEvent) => {
    e.stopPropagation()
    navigator.clipboard.writeText(keyCode).then(() => {
      setCopied(true)
      setTimeout(() => setCopied(false), 1500)
    })
  }

  return (
    <div style={{ display: 'flex', alignItems: 'center', gap: 4 }}>
      <code style={{ fontSize: 11, color: '#9ca3af', wordBreak: 'break-all', flex: 1 }}>
        {visible ? keyCode : maskKey(keyCode)}
      </code>
      <button
        onClick={e => { e.stopPropagation(); setVisible(v => !v) }}
        title={visible ? '隐藏' : '显示'}
        style={iconBtnStyle}
      >
        {visible ? <EyeOff size={13} /> : <Eye size={13} />}
      </button>
      <button onClick={handleCopy} title="复制" style={iconBtnStyle}>
        {copied ? <Check size={13} color="#16a34a" /> : <Copy size={13} />}
      </button>
    </div>
  )
}

const iconBtnStyle: React.CSSProperties = {
  background: 'none', border: 'none', cursor: 'pointer',
  padding: 3, borderRadius: 4, color: '#9ca3af',
  display: 'flex', alignItems: 'center', flexShrink: 0,
}

function InlineEditName({ value, onSave }: { value: string; onSave: (name: string) => void }) {
  const [editing, setEditing] = useState(false)
  const [draft, setDraft] = useState(value)
  const inputRef = useRef<HTMLInputElement>(null)

  const start = () => { setDraft(value); setEditing(true); setTimeout(() => inputRef.current?.focus(), 0) }
  const cancel = () => setEditing(false)
  const save = () => {
    const trimmed = draft.trim()
    if (trimmed && trimmed !== value) onSave(trimmed)
    setEditing(false)
  }

  if (editing) {
    return (
      <div style={{ display: 'flex', alignItems: 'center', gap: 4 }}>
        <input
          ref={inputRef}
          value={draft}
          onChange={e => setDraft(e.target.value)}
          onKeyDown={e => { if (e.key === 'Enter') save(); if (e.key === 'Escape') cancel() }}
          style={{ ...inputStyle, padding: '2px 6px', fontSize: 13, width: 120 }}
        />
        <button onClick={save} style={iconBtnStyle} title="保存"><Check size={13} color="#16a34a" /></button>
        <button onClick={cancel} style={iconBtnStyle} title="取消"><X size={13} /></button>
      </div>
    )
  }

  return (
    <div style={{ display: 'flex', alignItems: 'center', gap: 4 }}>
      <span style={{ fontWeight: 500 }}>{value}</span>
      <button onClick={start} style={iconBtnStyle} title="修改名称"><Pencil size={12} /></button>
    </div>
  )
}
