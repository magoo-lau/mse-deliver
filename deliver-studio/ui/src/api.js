// 轻量 API 封装（fetch）：JSON 与 FormData 两种载荷，错误统一弹 ElMessage；
// 全量携带登录 token（localStorage），401（未登录/会话过期）自动清 token 回登录页
import { ElMessage } from 'element-plus'
import { authed } from './nav'

const TOKEN_KEY = 'studio_token'

export function studioToken() {
  return localStorage.getItem(TOKEN_KEY) || ''
}

export function setStudioToken(token) {
  if (token) localStorage.setItem(TOKEN_KEY, token)
  else localStorage.removeItem(TOKEN_KEY)
}

function withAuth(opts) {
  const t = studioToken()
  if (t) opts.headers = { ...(opts.headers || {}), 'X-Studio-Token': t }
}

async function request(method, url, body) {
  const opts = { method }
  withAuth(opts)
  if (body instanceof FormData) {
    opts.body = body
  } else if (body !== undefined) {
    opts.headers = { ...(opts.headers || {}), 'Content-Type': 'application/json' }
    opts.body = JSON.stringify(body)
  }
  const resp = await fetch(url, opts)
  if (resp.status === 401) {
    setStudioToken('')
    authed.value = false
    throw new Error('未登录或会话已过期，请重新登录')
  }
  if (!resp.ok) {
    let msg = `${resp.status}`
    try {
      const data = await resp.json()
      if (data && data.error) msg = data.error
    } catch { /* 非 JSON 错误体 */ }
    throw new Error(msg)
  }
  return resp.json()
}

export const api = {
  get: (url) => request('GET', url),
  post: (url, body) => request('POST', url, body),
  put: (url, body) => request('PUT', url, body),
  del: (url) => request('DELETE', url),
  upload: (url, formData) => request('POST', url, formData)
}

export function fmtSize(size) {
  if (!size) return '-'
  if (size > 1024 * 1024 * 1024) return (size / 1024 / 1024 / 1024).toFixed(2) + ' GB'
  if (size > 1024 * 1024) return (size / 1024 / 1024).toFixed(1) + ' MB'
  return (size / 1024).toFixed(1) + ' KB'
}

// 大文件下载（GB 级套件/交付包）：登录态换一次性下载票 → 浏览器原生下载器直 GET。
// fetch+blob 会把整包读进内存（实测 1.6GB 即「点击无反应」），原生下载带进度、流式落盘零内存压力；
// 文件名由后端 Content-Disposition 提供（同一性防篡改）。
export async function downloadViaTicket(path) {
  const res = await api.post(`${path}/ticket`)
  if (!res || !res.url) throw new Error('下载票签发失败')
  const a = document.createElement('a')
  a.href = res.url
  document.body.appendChild(a)
  a.click()
  a.remove()
}

// 下载 GET 端点到本地文件（后端带 Content-Disposition 时优先用其中的文件名）
export async function downloadFile(url, fallbackName) {
  const opts = { method: 'GET' }
  withAuth(opts)
  const resp = await fetch(url, opts)
  if (resp.status === 401) {
    setStudioToken('')
    authed.value = false
    throw new Error('未登录或会话已过期，请重新登录')
  }
  if (!resp.ok) {
    let msg = `${resp.status}`
    try {
      const data = await resp.json()
      if (data && data.error) msg = data.error
    } catch { /* 非 JSON 错误体 */ }
    throw new Error(msg)
  }
  let name = fallbackName
  const cd = resp.headers.get('Content-Disposition') || ''
  const m = cd.match(/filename="?([^";]+)"?/)
  if (m) name = m[1]
  const blob = await resp.blob()
  const a = document.createElement('a')
  a.href = URL.createObjectURL(blob)
  a.download = name || 'download'
  a.click()
  URL.revokeObjectURL(a.href)
}
