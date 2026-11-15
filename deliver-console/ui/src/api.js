// 轻量 API 封装（fetch）：JSON 与 FormData 两种载荷，错误统一弹 ElMessage；
// 全量携带登录 token（localStorage），401 自动清 token 回登录页
import { ElMessage } from 'element-plus'
import { authed } from './nav'

const TOKEN_KEY = 'console_token'

export function consoleToken() {
  return localStorage.getItem(TOKEN_KEY) || ''
}

export function setConsoleToken(token) {
  if (token) localStorage.setItem(TOKEN_KEY, token)
  else localStorage.removeItem(TOKEN_KEY)
}

function withAuth(opts) {
  const t = consoleToken()
  if (t) opts.headers = { ...(opts.headers || {}), 'X-Console-Token': t }
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
    setConsoleToken('')
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

// 上传走 XHR（fetch 没有上传进度回调，大部署包需要进度反馈）；鉴权/401 语义与 request 一致
function uploadXHR(url, formData, onProgress) {
  return new Promise((resolve, reject) => {
    const xhr = new XMLHttpRequest()
    xhr.open('POST', url)
    const t = consoleToken()
    if (t) xhr.setRequestHeader('X-Console-Token', t)
    xhr.upload.onprogress = (e) => {
      if (e.lengthComputable && onProgress) onProgress(Math.round((e.loaded / e.total) * 100))
    }
    xhr.onerror = () => reject(new Error('网络错误（上传中断）'))
    xhr.onload = () => {
      if (xhr.status === 401) {
        setConsoleToken('')
        authed.value = false
        reject(new Error('未登录或会话已过期，请重新登录'))
        return
      }
      let body = {}
      try { body = JSON.parse(xhr.responseText) } catch { /* 非 JSON 错误体 */ }
      if (xhr.status >= 200 && xhr.status < 300) resolve(body)
      else reject(new Error(body.error || `${xhr.status}`))
    }
    xhr.send(formData)
  })
}

export const api = {
  get: (url) => request('GET', url),
  post: (url, body) => request('POST', url, body),
  put: (url, body) => request('PUT', url, body),
  del: (url) => request('DELETE', url),
  upload: (url, formData, onProgress) => uploadXHR(url, formData, onProgress)
}

export function fmtTime(unix) {
  if (!unix) return '-'
  const d = new Date(unix * 1000)
  const p = (n) => String(n).padStart(2, '0')
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}:${p(d.getSeconds())}`
}

export function downloadWithToken(url, saveName) {
  // fetch 带 token 落 blob（<a download> 直链带不了自定义头）
  fetch(url, { headers: { 'X-Console-Token': consoleToken() } }).then((r) => {
    if (!r.ok) throw new Error(`下载失败（${r.status}）`)
    return r.blob()
  }).then((b) => {
    const a = document.createElement('a')
    a.href = URL.createObjectURL(b)
    a.download = saveName
    a.click()
    URL.revokeObjectURL(a.href)
  }).catch((e) => ElMessage.error(e.message))
}
