// YAML 文档（配置模板级）解析/序列化：{{X}} 占位符经私有区字符保护后走 js-yaml
// （裸 {{X}} 会被 YAML 当嵌套 flow mapping 解析出垃圾结构；与 composeYaml.js 同款手法，抽公共件）
import yaml from 'js-yaml'

const PH = ''
const PH_TEXT_RE = /\{\{(\w+)\}\}/g
const PH_MARK_RE = new RegExp(PH + '(\\w+)' + PH, 'g')
const DUMP_OPTS = { indent: 2, lineWidth: -1, noRefs: true }

function protectStr(s) {
  return s.replace(PH_TEXT_RE, PH + '$1' + PH)
}

function deepRestore(v) {
  if (typeof v === 'string') return v.replace(PH_MARK_RE, '{{$1}}')
  if (Array.isArray(v)) return v.map(deepRestore)
  if (v && typeof v === 'object') {
    for (const k of Object.keys(v)) v[k] = deepRestore(v[k])
    return v
  }
  return v
}

function deepProtect(v) {
  if (typeof v === 'string') return protectStr(v)
  if (Array.isArray(v)) return v.map(deepProtect)
  if (v && typeof v === 'object') {
    const o = {}
    for (const k of Object.keys(v)) o[k] = deepProtect(v[k])
    return o
  }
  return v
}

// 文本 → 根 map 对象；空文本 = {}；解析失败或根不是 map（多文档/纯标量/数组根）= null（调用方退文本视图）
export function parseDoc(text) {
  const t = String(text || '')
  if (!t.trim()) return {}
  let doc
  try {
    doc = yaml.load(protectStr(t))
  } catch {
    return null
  }
  if (doc == null) return {}
  if (typeof doc !== 'object' || Array.isArray(doc)) return null
  return deepRestore(JSON.parse(JSON.stringify(doc)))
}

// 根 map 对象 → 文本（占位符保护后 dump 再还原；lineWidth -1 防长行折断 URL/表达式）
export function dumpDoc(model) {
  if (!model || typeof model !== 'object' || Array.isArray(model) || !Object.keys(model).length) return ''
  return yaml.dump(deepProtect(JSON.parse(JSON.stringify(model))), DUMP_OPTS).replace(PH_MARK_RE, '{{$1}}')
}

// 任意值（数组叶子用）：文本解析须为数组，否则 null
export function parseArray(text) {
  const t = String(text || '').trim()
  if (!t) return []
  let v
  try {
    v = yaml.load(protectStr(t))
  } catch {
    return null
  }
  if (!Array.isArray(v)) return null
  return deepRestore(JSON.parse(JSON.stringify(v)))
}

// 任意值 → 文本（数组叶子编辑块）
export function dumpValue(v) {
  if (v == null) return ''
  return yaml.dump(deepProtect(JSON.parse(JSON.stringify(v))), DUMP_OPTS)
    .replace(PH_MARK_RE, '{{$1}}')
    .replace(/\n+$/, '\n')
}
