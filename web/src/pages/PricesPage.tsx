import { useEffect, useRef, useState } from 'react'
import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query'
import { listPrices, setPrices, deletePrices, ocrPrice, type ModelPrice, type OCRResult } from '../lib/api'

export default function PricesPage() {
  const qc = useQueryClient()
  const [editModel, setEditModel] = useState<string | null>(null)
  const [newModel, setNewModel] = useState('')
  const [ocrPreview, setOcrPreview] = useState<OCRResult | null>(null)
  const [ocrImageURL, setOcrImageURL] = useState<string>('')
  const [ocrLoading, setOcrLoading] = useState(false)
  const [ocrError, setOcrError] = useState('')
  const fileInputRef = useRef<HTMLInputElement>(null)

  const { data: pricesMap = {} } = useQuery({ queryKey: ['prices'], queryFn: listPrices })

  const saveMut = useMutation({
    mutationFn: ({ model, prices }: { model: string; prices: ModelPrice[] }) => setPrices(model, prices),
    onSuccess: () => qc.invalidateQueries({ queryKey: ['prices'] }),
  })
  const deleteMut = useMutation({
    mutationFn: deletePrices,
    onSuccess: () => qc.invalidateQueries({ queryKey: ['prices'] }),
  })

  // Revoke the object URL when the component unmounts or when ocrImageURL changes.
  useEffect(() => {
    return () => { if (ocrImageURL) URL.revokeObjectURL(ocrImageURL) }
  }, [ocrImageURL])

  const runOcr = async (file: File) => {
    setOcrError('')
    setOcrLoading(true)
    const url = URL.createObjectURL(file)
    setOcrImageURL(prev => { if (prev) URL.revokeObjectURL(prev); return url })
    try {
      const result = await ocrPrice(file)
      setOcrPreview(result)
    } catch (err: unknown) {
      const msg = err instanceof Error ? err.message : '识别失败'
      setOcrError(msg)
    } finally {
      setOcrLoading(false)
    }
  }

  const handleFileChange = async (e: React.ChangeEvent<HTMLInputElement>) => {
    const file = e.target.files?.[0]
    if (!file) return
    e.target.value = ''
    await runOcr(file)
  }

  const handlePaste = async (e: React.ClipboardEvent) => {
    const item = Array.from(e.clipboardData.items).find(i => i.type.startsWith('image/'))
    if (!item) return
    const file = item.getAsFile()
    if (!file) return
    await runOcr(file)
  }

  const confirmOcr = () => {
    if (!ocrPreview) return
    const sorted = [...ocrPreview.prices].sort((a, b) => a.context_min - b.context_min)
    saveMut.mutate({ model: ocrPreview.model, prices: sorted })
    setEditModel(ocrPreview.model)
    setOcrPreview(null)
  }

  const models = Object.keys(pricesMap)

  return (
    <div onPaste={handlePaste}>
      <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', marginBottom: 16 }}>
        <h2 style={{ margin: 0, fontSize: 16 }}>模型单价配置</h2>
        <div style={{ display: 'flex', gap: 8, alignItems: 'center' }}>
          <input
            placeholder="模型名称"
            value={newModel}
            onChange={e => setNewModel(e.target.value)}
            style={{ padding: '5px 10px', border: '1px solid #d1d5db', borderRadius: 6, fontSize: 13 }}
          />
          <button
            onClick={() => {
              if (!newModel.trim()) return
              saveMut.mutate({ model: newModel.trim(), prices: [defaultTier(newModel.trim())] })
              setEditModel(newModel.trim())
              setNewModel('')
            }}
            style={btnStyle('blue')}
          >
            + 添加模型
          </button>
          <input
            ref={fileInputRef}
            type="file"
            accept="image/*"
            style={{ display: 'none' }}
            onChange={handleFileChange}
          />
          <button
            onClick={() => fileInputRef.current?.click()}
            disabled={ocrLoading}
            style={btnStyle('gray')}
          >
            {ocrLoading ? '识别中…' : '截图导入'}
          </button>
          {!ocrLoading && (
            <span style={{ fontSize: 12, color: '#9ca3af' }}>或 Ctrl+V 粘贴截图</span>
          )}
        </div>
      </div>

      {ocrError && (
        <div style={{ marginBottom: 12, padding: '8px 12px', background: '#fee2e2', color: '#991b1b', borderRadius: 6, fontSize: 13 }}>
          识别失败：{ocrError}
        </div>
      )}

      {ocrPreview && (
        <div style={{ marginBottom: 16, background: '#f0f9ff', border: '1px solid #bae6fd', borderRadius: 8, padding: 16 }}>
          <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', marginBottom: 12 }}>
            <span style={{ fontWeight: 600, fontSize: 14 }}>识别结果预览 — {ocrPreview.model}</span>
            <div style={{ display: 'flex', gap: 8 }}>
              <button onClick={confirmOcr} style={btnStyle('blue')}>确认保存</button>
              <button onClick={() => setOcrPreview(null)} style={btnStyle('gray')}>取消</button>
            </div>
          </div>
          {ocrImageURL && (
            <img
              src={ocrImageURL}
              alt="原始截图"
              style={{ maxWidth: '100%', maxHeight: 300, borderRadius: 6, border: '1px solid #bae6fd', marginBottom: 12, display: 'block' }}
            />
          )}
          <table style={{ width: '100%', borderCollapse: 'collapse', fontSize: 12 }}>
            <thead>
              <tr style={{ background: '#e0f2fe' }}>
                {['context_min', 'context_max', '输入¥/M', '输出¥/M', '缓存命中¥/M', '缓存创建¥/M'].map(h => (
                  <th key={h} style={{ padding: '6px 8px', textAlign: 'left', borderBottom: '1px solid #bae6fd' }}>{h}</th>
                ))}
              </tr>
            </thead>
            <tbody>
              {ocrPreview.prices.map((p, i) => (
                <tr key={i}>
                  <td style={{ padding: '4px 8px' }}>{p.context_min}</td>
                  <td style={{ padding: '4px 8px' }}>{p.context_max === -1 || p.context_max == null ? '∞' : p.context_max}</td>
                  <td style={{ padding: '4px 8px' }}>{p.input_cny}</td>
                  <td style={{ padding: '4px 8px' }}>{p.output_cny}</td>
                  <td style={{ padding: '4px 8px' }}>{p.cache_hit_cny && p.cache_hit_cny !== '0' ? p.cache_hit_cny : '—'}</td>
                  <td style={{ padding: '4px 8px' }}>{p.cache_write_cny && p.cache_write_cny !== '0' ? p.cache_write_cny : '—'}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}

      {models.length === 0 && (
        <p style={{ color: '#6b7280', fontSize: 14 }}>暂无配置，添加模型后设置单价。</p>
      )}

      {models.map(model => (
        <div key={model} style={{ background: '#fff', border: '1px solid #e5e7eb', borderRadius: 8, marginBottom: 12 }}>
          <div
            style={{ padding: '10px 16px', display: 'flex', justifyContent: 'space-between', alignItems: 'center', cursor: 'pointer' }}
            onClick={() => setEditModel(editModel === model ? null : model)}
          >
            <span style={{ fontWeight: 600 }}>{model}</span>
            <div style={{ display: 'flex', gap: 8 }}>
              <span style={{ fontSize: 12, color: '#6b7280' }}>{pricesMap[model].length} 档</span>
              <button onClick={e => { e.stopPropagation(); deleteMut.mutate(model) }} style={btnStyle('red')}>删除</button>
            </div>
          </div>

          {editModel === model && (
            <PriceTiersEditor
              model={model}
              tiers={pricesMap[model]}
              onSave={prices => saveMut.mutate({ model, prices })}
            />
          )}
        </div>
      ))}
    </div>
  )
}

function PriceTiersEditor({ model, tiers, onSave }: {
  model: string
  tiers: ModelPrice[]
  onSave: (prices: ModelPrice[]) => void
}) {
  const [rows, setRows] = useState<ModelPrice[]>(tiers)

  // Sync local rows when the server data is refreshed after a save.
  useEffect(() => { setRows(tiers) }, [tiers])

  const PRICE_FIELDS = new Set(['input_cny', 'output_cny', 'cache_hit_cny', 'cache_write_cny'])

  const update = (i: number, field: keyof ModelPrice, value: string) => {
    setRows(prev => prev.map((r, idx) => {
      if (idx !== i) return r
      if (field === 'context_max') {
        return { ...r, context_max: value.trim() === '' ? -1 : parseInt(value) }
      }
      if (value === '') return { ...r, [field]: undefined }
      if (field === 'context_min') {
        return { ...r, [field]: parseInt(value) }
      }
      if (PRICE_FIELDS.has(field)) {
        // Store as-is: value is already in yuan/百万token
        return { ...r, [field]: value }
      }
      return { ...r, [field]: parseFloat(value) }
    }))
  }

  return (
    <div style={{ padding: '0 16px 16px' }}>
      <table style={{ width: '100%', borderCollapse: 'collapse', fontSize: 12 }}>
        <thead>
          <tr style={{ background: '#f9fafb' }}>
            {['context_min', 'context_max(空=无上限)', '输入¥/M', '输出¥/M', '缓存命中¥/M', '缓存创建¥/M', ''].map(h => (
              <th key={h} style={{ padding: '6px 8px', textAlign: 'left', borderBottom: '1px solid #e5e7eb' }}>{h}</th>
            ))}
          </tr>
        </thead>
        <tbody>
          {rows.map((row, i) => (
            <tr key={row.id}>
              {(['context_min', 'context_max', 'input_cny', 'output_cny', 'cache_hit_cny', 'cache_write_cny'] as const).map(field => {
                const isPriceField = ['input_cny', 'output_cny', 'cache_hit_cny', 'cache_write_cny'].includes(field)
                const raw = row[field]
                const displayVal = raw == null ? ''
                  : field === 'context_max' && raw === -1 ? ''
                  : raw
                return (
                  <td key={field} style={{ padding: '4px 4px' }}>
                    <input
                      type="number"
                      step={isPriceField ? '0.001' : '1'}
                      value={displayVal}
                      onChange={e => update(i, field, e.target.value)}
                      style={{ padding: '4px 6px', border: '1px solid #d1d5db', borderRadius: 4, fontSize: 12, width: '100%', boxSizing: 'border-box' }}
                    />
                  </td>
                )
              })}
              <td style={{ padding: '4px 4px' }}>
                <button onClick={() => setRows(prev => prev.filter((_, idx) => idx !== i))} style={btnStyle('red')}>删</button>
              </td>
            </tr>
          ))}
        </tbody>
      </table>
      <div style={{ display: 'flex', gap: 8, marginTop: 8 }}>
        <button onClick={() => setRows(prev => [...prev, defaultTier(model)])} style={btnStyle('gray')}>+ 加档</button>
        <button onClick={() => onSave(rows)} style={btnStyle('blue')}>保存</button>
      </div>
    </div>
  )
}

function defaultTier(model: string): ModelPrice {
  return { id: 0, model, context_min: 0, context_max: -1, input_cny: '0', output_cny: '0' }
}

function btnStyle(color: 'blue' | 'red' | 'gray'): React.CSSProperties {
  const colors = {
    blue: { background: '#2563eb', color: '#fff' },
    red: { background: '#fee2e2', color: '#991b1b' },
    gray: { background: '#f3f4f6', color: '#374151' },
  }
  return { ...colors[color], border: 'none', padding: '4px 12px', borderRadius: 6, fontSize: 12, cursor: 'pointer' }
}
