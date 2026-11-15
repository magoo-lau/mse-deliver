// CodeMirror 共享工具：刷新注解 / {{变量}} 装饰高亮 / 变量补全源（模板编辑器与编排块编辑器共用）
import { Annotation } from '@codemirror/state'
import { Decoration, ViewPlugin } from '@codemirror/view'

// 外部数据（如变量字典）变化时，用该注解 dispatch 空事务驱动装饰重建
export const RefreshAnnotation = Annotation.define()

// {{X}} 占位高亮：已登记变量 ph-ok / 未定义引用 ph-bad（配色沿用 TemplateEditor 旧版）
export function varHighlightExtension(getKnownNames) {
  return ViewPlugin.fromClass(
    class {
      constructor(view) {
        this.decorations = build(view)
      }
      update(u) {
        if (u.docChanged || u.viewportChanged || u.transactions.some((t) => t.annotation(RefreshAnnotation))) {
          this.decorations = build(u)
        }
      }
    },
    { decorations: (v) => v.decorations }
  )
  function build(view) {
    const known = new Set(getKnownNames() || [])
    const ranges = []
    const re = /\{\{[A-Za-z0-9_]+\}\}/g
    for (const { from, to } of view.visibleRanges) {
      const text = view.state.sliceDoc(from, to)
      let m
      while ((m = re.exec(text))) {
        const start = from + m.index
        const name = m[0].slice(2, -2)
        ranges.push(Decoration.mark({ class: known.has(name) ? 'ph-ok' : 'ph-bad' }).range(start, start + m[0].length))
      }
    }
    return Decoration.set(ranges, true)
  }
}

// {{变量名}} 补全：输入 {{ 后的前缀触发；apply 智能闭合 —— 光标后已有 }} 时只插名字
export function varCompletionSource(getVars) {
  return (ctx) => {
    const line = ctx.state.doc.lineAt(ctx.pos)
    const before = line.text.slice(0, ctx.pos - line.from)
    const m = before.match(/\{\{(\w*)$/)
    if (!m) return null
    const vars = getVars() || []
    if (!vars.length) return null
    return {
      from: ctx.pos - m[1].length,
      options: vars.map((v) => ({
        label: v.name,
        detail: [v.label, v.origin, v.when === 'build' ? '打包时' : v.when === 'install' ? '安装时' : ''].filter(Boolean).join(' · '),
        type: 'variable',
        apply(view, completion, from, to) {
          const after = view.state.sliceDoc(to, to + 2)
          view.dispatch({ changes: { from, to, insert: after === '}}' ? completion.label : completion.label + '}}' } })
        }
      })),
      validFor: /^\w*$/
    }
  }
}
