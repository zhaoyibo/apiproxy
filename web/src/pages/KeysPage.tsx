import { useEffect, useMemo, useRef, useState } from 'react'
import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query'
import { Eye, EyeOff, Copy, Check, Pencil, X, ArrowUp, ArrowDown, ChevronDown, ChevronRight } from 'lucide-react'
import {
  listRootKeys, listAllChildKeys, createKey, updateKey, deleteKey, clearExhausted, getAllStats,
  setRootChildren,
  type APIKey,
} from '../lib/api'

type DateRangeKey = 'today' | '24h' | '7d' | '30d' | 'thisMonth' | 'lastMonth'

const dateRangeOptions: { key: DateRangeKey; label: string }[] = [
  { key: 'today', label: '今天' },
  { key: '24h', label: '近24h' },
  { key: '7d', label: '近7天' },
  { key: '30d', label: '近30天' },
  { key: 'thisMonth', label: '本月' },
  { key: 'lastMonth', label: '上月' },
]

function toDateStr(d: Date): string {
  return d.toISOString().slice(0, 10)
}

function computeDateRange(key: DateRangeKey): { start: string; end: string } {
  const now = new Date()
  const today = toDateStr(now)
  switch (key) {
    case 'today':
      return { start: today, end: today }
    case '24h':
      // daily_stats is day-granular; approximate the last 24h as yesterday+today.
      return { start: toDateStr(new Date(now.getTime() - 86400_000)), end: today }
    case '7d':
      return { start: toDateStr(new Date(now.getTime() - 7 * 86400_000)), end: today }
    case '30d':
      return { start: toDateStr(new Date(now.getTime() - 30 * 86400_000)), end: today }
    case 'thisMonth':
      return { start: toDateStr(new Date(now.getFullYear(), now.getMonth(), 1)), end: today }
    case 'lastMonth': {
      const lastMonthStart = new Date(now.getFullYear(), now.getMonth() - 1, 1)
      const lastMonthEnd = new Date(now.getFullYear(), now.getMonth(), 0)
      return { start: toDateStr(lastMonthStart), end: toDateStr(lastMonthEnd) }
    }
  }
}

export default function KeysPage() {
  const qc = useQueryClient()
  const [dateRange, setDateRange] = useState<DateRangeKey>('today')
  // Recompute on every render so "今天" stays fresh if the page is left open overnight.
  const { start: rangeStart, end: rangeEnd } = computeDateRange(dateRange)

  const [showCreateRoot, setShowCreateRoot] = useState(false)
  const [showCreateChild, setShowCreateChild] = useState(false)
  const [editingChildId, setEditingChildId] = useState<number | null>(null)
  const [linkingRootId, setLinkingRootId] = useState<number | null>(null)
  const [rootsCollapsed, setRootsCollapsed] = useState(true)
  const [childrenCollapsed, setChildrenCollapsed] = useState(true)
  const [statsTab, setStatsTab] = useState<'all' | number>('all')

  const { data: rootKeys = [] } = useQuery({ queryKey: ['rootKeys'], queryFn: listRootKeys, refetchInterval: 30_000 })
  const { data: childKeys = [] } = useQuery({ queryKey: ['childKeys'], queryFn: listAllChildKeys, refetchInterval: 30_000 })
  const { data: stats = [] } = useQuery({
    queryKey: ['stats', rangeStart, rangeEnd],
    queryFn: () => getAllStats(rangeStart, rangeEnd),
    refetchInterval: 30_000,
  })

  const invalidateAll = () => {
    qc.invalidateQueries({ queryKey: ['rootKeys'] })
    qc.invalidateQueries({ queryKey: ['childKeys'] })
  }

  const createMut = useMutation({ mutationFn: createKey, onSuccess: invalidateAll })
  const updateMut = useMutation({
    mutationFn: ({ id, data }: { id: number; data: Partial<APIKey> }) => updateKey(id, data),
    onSuccess: invalidateAll,
  })
  const deleteMut = useMutation({ mutationFn: (id: number) => deleteKey(id), onSuccess: invalidateAll })
  const clearExhaustedMut = useMutation({
    mutationFn: (id: number) => clearExhausted(id),
    onSuccess: () => qc.invalidateQueries({ queryKey: ['rootKeys'] }),
  })
  const linkMut = useMutation({
    mutationFn: ({ rootId, childIds }: { rootId: number; childIds: number[] }) => setRootChildren(rootId, childIds),
    onSuccess: invalidateAll,
  })

  const rootNameMap = useMemo(() =>
    Object.fromEntries(rootKeys.map(k => [k.id, k.name])) as Record<number, string>,
    [rootKeys]
  )
  const childKeyMap = useMemo(() =>
    Object.fromEntries(childKeys.map(k => [k.id, k.name])) as Record<number, string>,
    [childKeys]
  )

  // Reset the child tab if its key disappears.
  useEffect(() => {
    if (statsTab !== 'all' && !childKeys.some(k => k.id === statsTab)) setStatsTab('all')
  }, [childKeys, statsTab])

  const visibleStats = useMemo(() =>
    statsTab === 'all' ? stats : stats.filter(s => s.key_id === statsTab),
    [stats, statsTab]
  )
  const costByChild = useMemo(() => {
    const m: Record<number, number> = {}
    for (const s of stats) m[s.key_id] = (m[s.key_id] ?? 0) + parseFloat(s.cost_cny)
    return m
  }, [stats])
  const totalCost = visibleStats.reduce((s, r) => s + parseFloat(r.cost_cny), 0)

  const editingChild = editingChildId != null ? childKeys.find(k => k.id === editingChildId) : undefined
  const linkingRoot = linkingRootId != null ? rootKeys.find(k => k.id === linkingRootId) : undefined

  return (
    <div style={{ display: 'flex', flexDirection: 'column', gap: 24 }}>
      {/* Root keys */}
      <div>
        <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', marginBottom: 10 }}>
          <h2
            onClick={() => setRootsCollapsed(v => !v)}
            style={{ margin: 0, fontSize: 15, fontWeight: 600, display: 'flex', alignItems: 'center', gap: 4, cursor: 'pointer', userSelect: 'none' }}
          >
            {rootsCollapsed ? <ChevronRight size={16} /> : <ChevronDown size={16} />}
            主 Key
            <span style={{ color: '#9ca3af', fontWeight: 400, fontSize: 13 }}>（{rootKeys.length}）</span>
          </h2>
          <Btn color="blue" onClick={() => { setRootsCollapsed(false); setShowCreateRoot(true) }}>+ 新建主 Key</Btn>
        </div>
        {!rootsCollapsed && (
        <>
        <div style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fill, minmax(280px, 1fr))', gap: 10 }}>
          {rootKeys.map(k => (
            <div key={k.id} style={{ background: '#fff', borderRadius: 10, border: '1px solid #e5e7eb', padding: '12px 14px' }}>
              <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', marginBottom: 6, gap: 8 }}>
                <span style={{ fontWeight: 600, fontSize: 14 }}>{k.name}</span>
                <div style={{ display: 'flex', gap: 6, alignItems: 'center' }}>
                  {k.exhausted && <ExhaustedBadge />}
                  <ActiveBadge active={k.is_active} />
                </div>
              </div>
              <div style={{ marginBottom: 6 }}>
                <KeyDisplay keyCode={k.key_code} />
              </div>
              <div style={{ fontSize: 12, color: '#9ca3af', marginBottom: 10 }}>
                额度(月): {k.quota_cny === '-1' || k.quota_cny == null ? '无限制' : `¥${parseFloat(k.quota_cny).toFixed(2)}`}
              </div>
              <div style={{ display: 'flex', gap: 6, flexWrap: 'wrap' }}>
                <Btn color="blue" onClick={() => setLinkingRootId(k.id)}>关联子 Key</Btn>
                <Btn color="ghost" onClick={() => updateMut.mutate({ id: k.id, data: { is_active: !k.is_active } })}>
                  {k.is_active ? '停用' : '启用'}
                </Btn>
                {k.exhausted && (
                  <Btn color="ghost" onClick={() => clearExhaustedMut.mutate(k.id)}>重置额度</Btn>
                )}
                <Btn color="danger" onClick={() => { if (confirm(`删除主 Key「${k.name}」？将从所有子 Key 解绑。`)) deleteMut.mutate(k.id) }}>删除</Btn>
              </div>
            </div>
          ))}
          {rootKeys.length === 0 && !showCreateRoot && (
            <p style={{ fontSize: 13, color: '#9ca3af', margin: 0 }}>暂无主 Key</p>
          )}
        </div>
        {showCreateRoot && (
          <div style={{ marginTop: 12, maxWidth: 360 }}>
            <RootKeyForm
              onSubmit={data => { createMut.mutate(data); setShowCreateRoot(false) }}
              onCancel={() => setShowCreateRoot(false)}
            />
          </div>
        )}
        </>
        )}
      </div>

      {/* Child keys */}
      <div>
        <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', marginBottom: 10 }}>
          <h2
            onClick={() => setChildrenCollapsed(v => !v)}
            style={{ margin: 0, fontSize: 15, fontWeight: 600, display: 'flex', alignItems: 'center', gap: 4, cursor: 'pointer', userSelect: 'none' }}
          >
            {childrenCollapsed ? <ChevronRight size={16} /> : <ChevronDown size={16} />}
            子 Key
            <span style={{ color: '#9ca3af', fontWeight: 400, fontSize: 13 }}>（{childKeys.length}）</span>
          </h2>
          <Btn color="blue" onClick={() => { setChildrenCollapsed(false); setShowCreateChild(true); setEditingChildId(null) }}>+ 新建子 Key</Btn>
        </div>
        {!childrenCollapsed && (
        <>
        {(showCreateChild || editingChild) && (
          <div style={{ marginBottom: 12, maxWidth: 520 }}>
            <ChildKeyForm
              roots={rootKeys}
              existing={editingChild}
              onSubmit={data => {
                if (editingChild) updateMut.mutate({ id: editingChild.id, data })
                else createMut.mutate(data)
                setShowCreateChild(false)
                setEditingChildId(null)
              }}
              onCancel={() => { setShowCreateChild(false); setEditingChildId(null) }}
            />
          </div>
        )}
        <div style={{ background: '#fff', borderRadius: 10, border: '1px solid #e5e7eb', overflow: 'hidden' }}>
          <table style={{ width: '100%', borderCollapse: 'collapse', fontSize: 13 }}>
            <thead>
              <tr style={{ background: '#f9fafb' }}>
                {['名称', 'Key', '关联主 Key（按优先级）', '配额 (¥)', '已用 (¥)', '状态', '操作'].map(h => (
                  <th key={h} style={thStyle}>{h}</th>
                ))}
              </tr>
            </thead>
            <tbody>
              {childKeys.map((k, i) => (
                <tr key={k.id} style={{ borderTop: i === 0 ? 'none' : '1px solid #f3f4f6' }}>
                  <td style={tdStyle}>
                    <InlineEditName value={k.name} onSave={name => updateMut.mutate({ id: k.id, data: { name } })} />
                  </td>
                  <td style={tdStyle}><KeyDisplay keyCode={k.key_code} /></td>
                  <td style={tdStyle}>
                    <div style={{ display: 'flex', gap: 4, flexWrap: 'wrap' }}>
                      {(k.root_ids ?? []).map((rid, idx) => (
                        <span key={rid} style={rootChipStyle}>
                          <span style={{ color: '#9ca3af', marginRight: 4 }}>{idx + 1}</span>
                          {rootNameMap[rid] ?? `#${rid}`}
                        </span>
                      ))}
                      {(k.root_ids ?? []).length === 0 && <span style={{ color: '#ef4444', fontSize: 12 }}>未关联</span>}
                    </div>
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
                      <Btn color="ghost" onClick={() => { setEditingChildId(k.id); setShowCreateChild(false) }}>关联</Btn>
                      <Btn color="ghost" onClick={() => updateMut.mutate({ id: k.id, data: { is_active: !k.is_active } })}>
                        {k.is_active ? '停用' : '启用'}
                      </Btn>
                      <Btn color="danger" onClick={() => { if (confirm(`删除子 Key「${k.name}」？`)) deleteMut.mutate(k.id) }}>删除</Btn>
                    </div>
                  </td>
                </tr>
              ))}
              {childKeys.length === 0 && (
                <tr>
                  <td colSpan={7} style={{ ...tdStyle, color: '#9ca3af', textAlign: 'center', padding: '20px 0' }}>
                    暂无子 Key
                  </td>
                </tr>
              )}
            </tbody>
          </table>
        </div>
        </>
        )}
      </div>

      {/* Stats */}
      <div>
        <SectionHeader
          title={
            <>
              用量统计
              <span style={{ fontWeight: 400, fontSize: 13, color: '#6b7280', marginLeft: 8 }}>
                共 ¥{totalCost.toFixed(6)}
              </span>
            </>
          }
        />
        {/* Time-range tabs */}
        <div style={{ display: 'flex', gap: 6, flexWrap: 'wrap', marginBottom: 10 }}>
          {dateRangeOptions.map(opt => (
            <button key={opt.key} onClick={() => setDateRange(opt.key)} style={tabStyle(dateRange === opt.key)}>
              {opt.label}
            </button>
          ))}
        </div>
        {/* Child-key tabs */}
        <div style={{ display: 'flex', gap: 6, flexWrap: 'wrap', marginBottom: 10 }}>
          <button onClick={() => setStatsTab('all')} style={tabStyle(statsTab === 'all')}>
            全部
            <span style={tabBadgeStyle(statsTab === 'all')}>
              ¥{stats.reduce((s, r) => s + parseFloat(r.cost_cny), 0).toFixed(4)}
            </span>
          </button>
          {Object.entries(costByChild).map(([kidStr, cost]) => {
            const kid = Number(kidStr)
            return (
              <button key={kid} onClick={() => setStatsTab(kid)} style={tabStyle(statsTab === kid)}>
                {childKeyMap[kid] ?? `#${kid}`}
                <span style={tabBadgeStyle(statsTab === kid)}>¥{cost.toFixed(4)}</span>
              </button>
            )
          })}
        </div>
        <div style={{ background: '#fff', borderRadius: 10, border: '1px solid #e5e7eb', overflow: 'hidden' }}>
          <table style={{ width: '100%', borderCollapse: 'collapse', fontSize: 13 }}>
            <thead>
              <tr style={{ background: '#f9fafb' }}>
                {[...(statsTab === 'all' ? ['子 Key'] : []), '日期', '模型', '成功', '失败', '输入 Token', '输出 Token', '缓存命中', '缓存创建', '费用 (¥)'].map(h => (
                  <th key={h} style={thStyle}>{h}</th>
                ))}
              </tr>
            </thead>
            <tbody>
              {visibleStats.length === 0 && (
                <tr>
                  <td colSpan={statsTab === 'all' ? 10 : 9} style={{ ...tdStyle, color: '#9ca3af', textAlign: 'center', padding: '20px 0' }}>
                    该时间段暂无数据
                  </td>
                </tr>
              )}
              {visibleStats.map((s, i) => (
                <tr key={s.id} style={{ borderTop: i === 0 ? 'none' : '1px solid #f3f4f6' }}>
                  {statsTab === 'all' && (
                    <td style={tdStyle}><span style={{ fontWeight: 500 }}>{childKeyMap[s.key_id] ?? `#${s.key_id}`}</span></td>
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

      {/* Batch-link children to a root (modal) */}
      {linkingRoot && (
        <Modal onClose={() => setLinkingRootId(null)}>
          <RootChildrenLinker
            root={linkingRoot}
            childList={childKeys}
            onSave={childIds => { linkMut.mutate({ rootId: linkingRoot.id, childIds }); setLinkingRootId(null) }}
            onCancel={() => setLinkingRootId(null)}
          />
        </Modal>
      )}
    </div>
  )
}

function Modal({ onClose, children }: { onClose: () => void; children: React.ReactNode }) {
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => { if (e.key === 'Escape') onClose() }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [onClose])
  return (
    <div
      onClick={onClose}
      style={{
        position: 'fixed', inset: 0, zIndex: 1000,
        background: 'rgba(17, 24, 39, 0.45)',
        display: 'flex', alignItems: 'center', justifyContent: 'center', padding: 16,
      }}
    >
      <div onClick={e => e.stopPropagation()} style={{ width: '100%', maxWidth: 520, maxHeight: '85vh', overflow: 'auto' }}>
        {children}
      </div>
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

function RootKeyForm({ onSubmit, onCancel }: {
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
        onSubmit({ name, key_code: keyCode || undefined, quota_cny: quota || undefined })
      }}
      style={{ background: '#f9fafb', borderRadius: 10, border: '1px solid #e5e7eb', padding: 14 }}
    >
      <div style={{ display: 'flex', flexDirection: 'column', gap: 8 }}>
        <input required placeholder="名称" value={name} onChange={e => setName(e.target.value)} style={inputStyle} />
        <input placeholder="Key（上游凭证，留空自动生成）" value={keyCode} onChange={e => setKeyCode(e.target.value)} style={inputStyle} />
        <input placeholder="月额度（元，留空无限制，仅参考）" type="number" step="0.001" value={quota} onChange={e => setQuota(e.target.value)} style={inputStyle} />
        <div style={{ display: 'flex', gap: 8 }}>
          <Btn color="blue" type="submit">创建</Btn>
          <Btn color="ghost" type="button" onClick={onCancel}>取消</Btn>
        </div>
      </div>
    </form>
  )
}

function ChildKeyForm({ roots, existing, onSubmit, onCancel }: {
  roots: APIKey[]
  existing?: APIKey
  onSubmit: (data: Partial<APIKey>) => void
  onCancel: () => void
}) {
  const [name, setName] = useState(existing?.name ?? '')
  const [keyCode, setKeyCode] = useState('')
  const [quota, setQuota] = useState(
    existing && existing.quota_cny && existing.quota_cny !== '-1' ? existing.quota_cny : ''
  )
  const [selectedRoots, setSelectedRoots] = useState<number[]>(existing?.root_ids ?? [])

  const isEdit = !!existing

  return (
    <form
      onSubmit={e => {
        e.preventDefault()
        if (selectedRoots.length === 0) { alert('请至少关联一个主 Key'); return }
        const data: Partial<APIKey> = { name, quota_cny: quota || undefined, root_ids: selectedRoots }
        if (!isEdit) data.key_code = keyCode || undefined
        onSubmit(data)
      }}
      style={{ background: '#f9fafb', borderRadius: 10, border: '1px solid #e5e7eb', padding: 14 }}
    >
      <div style={{ display: 'flex', flexDirection: 'column', gap: 10 }}>
        <input required placeholder="名称" value={name} onChange={e => setName(e.target.value)} style={inputStyle} />
        {!isEdit && (
          <input placeholder="Key（留空自动生成）" value={keyCode} onChange={e => setKeyCode(e.target.value)} style={inputStyle} />
        )}
        <input placeholder="配额（元，留空无限制）" type="number" step="0.001" value={quota} onChange={e => setQuota(e.target.value)} style={inputStyle} />
        <div>
          <div style={{ fontSize: 12, color: '#6b7280', marginBottom: 6 }}>关联主 Key（按优先级顺序，用尽自动切换到下一个）</div>
          <RootMultiSelect roots={roots} selected={selectedRoots} onChange={setSelectedRoots} />
        </div>
        <div style={{ display: 'flex', gap: 8 }}>
          <Btn color="blue" type="submit">{isEdit ? '保存' : '创建'}</Btn>
          <Btn color="ghost" type="button" onClick={onCancel}>取消</Btn>
        </div>
      </div>
    </form>
  )
}

function RootChildrenLinker({ root, childList, onSave, onCancel }: {
  root: APIKey
  childList: APIKey[]
  onSave: (childIds: number[]) => void
  onCancel: () => void
}) {
  // Initialise checked from which children currently include this root.
  const [checked, setChecked] = useState<Set<number>>(
    () => new Set(childList.filter(c => (c.root_ids ?? []).includes(root.id)).map(c => c.id))
  )
  const toggle = (id: number) => setChecked(prev => {
    const next = new Set(prev)
    if (next.has(id)) next.delete(id); else next.add(id)
    return next
  })

  return (
    <div style={{ background: '#fff', borderRadius: 12, border: '1px solid #e5e7eb', padding: 18, boxShadow: '0 10px 40px rgba(0,0,0,0.15)' }}>
      <div style={{ fontSize: 15, fontWeight: 600, marginBottom: 4 }}>
        关联子 Key <span style={{ color: '#6b7280', fontWeight: 400 }}>— {root.name}</span>
      </div>
      <div style={{ fontSize: 12, color: '#6b7280', marginBottom: 10 }}>
        勾选的子 Key 会把「{root.name}」追加到其失败切换列表末尾；取消勾选则解绑。其余关联与优先级不变。
      </div>
      <div style={{ display: 'flex', flexDirection: 'column', gap: 2, maxHeight: 260, overflowY: 'auto', marginBottom: 12 }}>
        {childList.map(c => (
          <label key={c.id} style={{ display: 'flex', alignItems: 'center', gap: 8, padding: '5px 6px', borderRadius: 6, cursor: 'pointer', fontSize: 13 }}>
            <input type="checkbox" checked={checked.has(c.id)} onChange={() => toggle(c.id)} />
            <span style={{ fontWeight: 500 }}>{c.name}</span>
            {(c.root_ids ?? []).length > 0 && (
              <span style={{ color: '#9ca3af', fontSize: 11 }}>（当前 {(c.root_ids ?? []).length} 个主 Key）</span>
            )}
          </label>
        ))}
        {childList.length === 0 && <span style={{ fontSize: 13, color: '#9ca3af' }}>暂无子 Key</span>}
      </div>
      <div style={{ display: 'flex', gap: 8 }}>
        <Btn color="blue" onClick={() => onSave([...checked])}>保存</Btn>
        <Btn color="ghost" onClick={onCancel}>取消</Btn>
      </div>
    </div>
  )
}

function RootMultiSelect({ roots, selected, onChange }: {
  roots: APIKey[]
  selected: number[]
  onChange: (ids: number[]) => void
}) {
  const rootNameMap = Object.fromEntries(roots.map(r => [r.id, r.name])) as Record<number, string>
  const unselected = roots.filter(r => !selected.includes(r.id))

  const move = (idx: number, dir: -1 | 1) => {
    const next = [...selected]
    const j = idx + dir
    if (j < 0 || j >= next.length) return
    ;[next[idx], next[j]] = [next[j], next[idx]]
    onChange(next)
  }
  const remove = (id: number) => onChange(selected.filter(x => x !== id))
  const add = (id: number) => onChange([...selected, id])

  return (
    <div style={{ display: 'flex', flexDirection: 'column', gap: 8 }}>
      {selected.length > 0 && (
        <div style={{ display: 'flex', flexDirection: 'column', gap: 4 }}>
          {selected.map((id, idx) => (
            <div key={id} style={{ display: 'flex', alignItems: 'center', gap: 6, background: '#eff6ff', border: '1px solid #bfdbfe', borderRadius: 6, padding: '4px 8px' }}>
              <span style={{ color: '#2563eb', fontWeight: 600, fontSize: 12, width: 16 }}>{idx + 1}</span>
              <span style={{ flex: 1, fontSize: 13 }}>{rootNameMap[id] ?? `#${id}`}</span>
              <button type="button" onClick={() => move(idx, -1)} disabled={idx === 0} style={iconBtnStyle} title="上移"><ArrowUp size={13} /></button>
              <button type="button" onClick={() => move(idx, 1)} disabled={idx === selected.length - 1} style={iconBtnStyle} title="下移"><ArrowDown size={13} /></button>
              <button type="button" onClick={() => remove(id)} style={iconBtnStyle} title="移除"><X size={13} /></button>
            </div>
          ))}
        </div>
      )}
      {unselected.length > 0 && (
        <div style={{ display: 'flex', gap: 6, flexWrap: 'wrap' }}>
          {unselected.map(r => (
            <button key={r.id} type="button" onClick={() => add(r.id)} style={{ ...rootChipStyle, cursor: 'pointer', border: '1px dashed #d1d5db' }}>
              + {r.name}
            </button>
          ))}
        </div>
      )}
      {roots.length === 0 && <span style={{ fontSize: 12, color: '#9ca3af' }}>请先创建主 Key</span>}
    </div>
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

function ExhaustedBadge() {
  return (
    <span style={{
      display: 'inline-block',
      fontSize: 11, padding: '2px 8px', borderRadius: 9999,
      background: '#fef3c7', color: '#b45309', fontWeight: 500,
    }}>
      本月额度已用尽
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

const rootChipStyle: React.CSSProperties = {
  display: 'inline-flex', alignItems: 'center',
  fontSize: 12, padding: '2px 8px', borderRadius: 9999,
  background: '#f3f4f6', color: '#374151',
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
