import { useRef, useState } from 'react'
import { useQueryClient } from '@tanstack/react-query'
import { exportConfig, importConfig, type ConfigExport, type ImportResult } from '../lib/api'

export default function ConfigPage() {
  const qc = useQueryClient()
  const fileInputRef = useRef<HTMLInputElement>(null)
  const [exporting, setExporting] = useState(false)
  const [importing, setImporting] = useState(false)
  const [importResult, setImportResult] = useState<ImportResult | null>(null)
  const [error, setError] = useState('')

  const handleExport = async () => {
    setError('')
    setExporting(true)
    try {
      const data = await exportConfig()
      const date = new Date().toISOString().slice(0, 10)
      const blob = new Blob([JSON.stringify(data, null, 2)], { type: 'application/json' })
      const url = URL.createObjectURL(blob)
      const a = document.createElement('a')
      a.href = url
      a.download = `apiproxy-config-${date}.json`
      a.click()
      // Delay revocation so the browser has time to initiate the download.
      setTimeout(() => URL.revokeObjectURL(url), 100)
    } catch (err) {
      setError(err instanceof Error ? err.message : '导出失败')
    } finally {
      setExporting(false)
    }
  }

  const handleFileChange = async (e: React.ChangeEvent<HTMLInputElement>) => {
    const file = e.target.files?.[0]
    if (!file) return
    e.target.value = ''

    const text = await file.text()
    let cfg: ConfigExport
    try {
      cfg = JSON.parse(text) as ConfigExport
    } catch {
      setError('文件不是有效的 JSON')
      return
    }
    if (typeof cfg !== 'object' || cfg === null || cfg.version == null || !Array.isArray(cfg.api_keys)) {
      setError('文件格式不正确，请确认是 apiproxy 导出的配置文件')
      return
    }

    const keyCount = cfg.api_keys?.length ?? 0
    const statCount = cfg.daily_stats?.length ?? 0
    const modelCount = Object.keys(cfg.model_prices ?? {}).length
    const confirmed = window.confirm(
      `导入将清空所有现有数据并覆盖，确认继续？\n\n` +
      `包含：${keyCount} 个 Key，${modelCount} 个模型价格，${statCount} 条用量统计`
    )
    if (!confirmed) return

    setError('')
    setImportResult(null)
    setImporting(true)
    try {
      const result = await importConfig(cfg)
      setImportResult(result)
      // Refresh all cached queries so Keys/Prices pages show fresh data.
      qc.invalidateQueries()
    } catch (err) {
      setError(err instanceof Error ? err.message : '导入失败')
    } finally {
      setImporting(false)
    }
  }

  return (
    <div style={{ maxWidth: 600 }}>
      <h2 style={{ margin: '0 0 24px', fontSize: 16 }}>配置导入导出</h2>

      {/* Export */}
      <section style={sectionStyle}>
        <h3 style={headingStyle}>导出</h3>
        <p style={descStyle}>
          将所有 API Keys、模型价格和用量统计导出为 JSON 文件，可用于备份或迁移。
        </p>
        <button onClick={handleExport} disabled={exporting} style={btnStyle('blue')}>
          {exporting ? '导出中…' : '下载配置文件'}
        </button>
      </section>

      {/* Import */}
      <section style={sectionStyle}>
        <h3 style={headingStyle}>导入</h3>
        <p style={descStyle}>
          从之前导出的 JSON 文件恢复配置。<strong>导入会清空并覆盖所有现有数据。</strong>
        </p>
        <input
          ref={fileInputRef}
          type="file"
          accept=".json,application/json"
          style={{ display: 'none' }}
          onChange={handleFileChange}
        />
        <button
          onClick={() => fileInputRef.current?.click()}
          disabled={importing}
          style={btnStyle('gray')}
        >
          {importing ? '导入中…' : '选择配置文件'}
        </button>

        {importResult && (
          <div style={{ marginTop: 12, padding: '10px 14px', background: '#f0fdf4', border: '1px solid #bbf7d0', borderRadius: 6, fontSize: 13 }}>
            导入成功：{importResult.keys} 个 Key，{importResult.models} 个模型价格，{importResult.stats} 条用量统计
          </div>
        )}
      </section>

      {error && (
        <div style={{ marginTop: 12, padding: '8px 12px', background: '#fee2e2', color: '#991b1b', borderRadius: 6, fontSize: 13 }}>
          {error}
        </div>
      )}
    </div>
  )
}

const sectionStyle: React.CSSProperties = {
  background: '#fff',
  border: '1px solid #e5e7eb',
  borderRadius: 10,
  padding: '20px 24px',
  marginBottom: 16,
}

const headingStyle: React.CSSProperties = {
  margin: '0 0 8px',
  fontSize: 14,
  fontWeight: 600,
}

const descStyle: React.CSSProperties = {
  margin: '0 0 14px',
  fontSize: 13,
  color: '#6b7280',
  lineHeight: 1.5,
}

function btnStyle(color: 'blue' | 'gray'): React.CSSProperties {
  return {
    padding: '7px 18px',
    borderRadius: 6,
    fontSize: 13,
    cursor: 'pointer',
    border: 'none',
    ...(color === 'blue'
      ? { background: '#2563eb', color: '#fff' }
      : { background: '#f3f4f6', color: '#374151', border: '1px solid #e5e7eb' }),
  }
}
