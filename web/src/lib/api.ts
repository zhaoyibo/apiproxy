import axios from 'axios'

const client = axios.create({
  baseURL: '/admin',
  withCredentials: true, // send session cookie automatically
})

// Redirect to login on 401
client.interceptors.response.use(
  r => r,
  err => {
    if (err.response?.status === 401 && window.location.pathname !== '/login') {
      window.location.href = '/login'
    }
    return Promise.reject(err)
  }
)

export const authClient = axios.create({ withCredentials: true })

export async function login(key: string): Promise<void> {
  await authClient.post('/auth/login', { key })
}

export async function logout(): Promise<void> {
  await authClient.post('/auth/logout')
  window.location.href = '/login'
}

export interface APIKey {
  id: number
  name: string
  key_code: string
  parent_id?: number
  quota_cny?: string  // yuan string; "-1" = unlimited
  used_cny: string    // yuan string, e.g. "0.000000030000"
  is_active: boolean
  created_at: string
}

export interface DailyStat {
  id: number
  date: string
  model: string
  input_tokens: number
  output_tokens: number
  cache_write_tokens: number
  cache_hit_tokens: number
  cost_cny: string  // yuan string, e.g. "0.000000030000"
}

export interface ModelPrice {
  id: number
  model: string
  context_min: number
  context_max?: number
  input_cny: string    // yuan/百万token, e.g. "0.3000"
  output_cny: string
  cache_hit_cny?: string
  cache_write_cny?: string
}

export const listRootKeys = () => client.get<APIKey[]>('/keys').then(r => r.data)
export const listChildKeys = (id: number) => client.get<APIKey[]>(`/keys/${id}/children`).then(r => r.data)
export const createKey = (data: Partial<APIKey>) => client.post<APIKey>('/keys', data).then(r => r.data)
export const updateKey = (id: number, data: Partial<APIKey>) => client.put<APIKey>(`/keys/${id}`, data).then(r => r.data)
export const deleteKey = (id: number) => client.delete(`/keys/${id}`)

export const getKeyStats = (id: number, start: string, end: string) =>
  client.get<DailyStat[]>(`/keys/${id}/stats`, { params: { start, end } }).then(r => r.data)

export const listPrices = () => client.get<Record<string, ModelPrice[]>>('/prices').then(r => r.data)
export const setPrices = (model: string, prices: ModelPrice[]) =>
  client.put<ModelPrice[]>(`/prices/${encodeURIComponent(model)}`, prices).then(r => r.data)
export const deletePrices = (model: string) => client.delete(`/prices/${encodeURIComponent(model)}`)

export interface ConfigExport {
  version: number
  exported_at: string
  model_prices: Record<string, ModelPrice[]>
  api_keys: APIKey[]
  daily_stats: DailyStat[]
}

export interface ImportResult {
  keys: number
  stats: number
  models: number
}

export const exportConfig = (): Promise<ConfigExport> =>
  client.get<ConfigExport>('/config/export').then(r => r.data)

export const importConfig = (cfg: ConfigExport): Promise<ImportResult> =>
  client.post<ImportResult>('/config/import', cfg).then(r => r.data)

export interface OCRResult {
  model: string
  prices: ModelPrice[]
}

export const ocrPrice = (file: File): Promise<OCRResult> => {
  const form = new FormData()
  form.append('image', file)
  // Let Axios set Content-Type automatically so it includes the multipart boundary.
  return client.post<OCRResult>('/prices/ocr', form).then(r => r.data)
}
