// The Workflow Code island — the console's ONE bundling seam (ADR-0012 §2,
// issue #416). Everything third-party (Monaco, monaco-yaml, the YAML
// tokenizer) is frozen by scripts/code-island/build.sh into a committed
// IIFE artifact; the application's own glue — schema fetch, theme mapping,
// e2e handle — lives in hand-written, unbundled static/js/code-island-glue.js
// and calls only the API this module exports.
//
// The island is deliberately dumb: it renders text, validates against the
// schema object it is handed, and reports markers. It never fetches (the
// glue owns /api/schema + the ETag dance) and never persists.
import * as monaco from 'monaco-editor/esm/vs/editor/editor.api'
// editor.all registers the CORE editor contributions (suggest controller,
// markers/squiggles, folding, find). editor.api alone ships a bare editor
// with no contributions — completion would silently never fire and
// validation markers would never render. This is the documented minimal
// recipe: api + all, then exactly one language.
import 'monaco-editor/esm/vs/editor/editor.all'
// yaml.contribution registers the monarch tokenizer for basic syntax
// highlighting; monaco-yaml registers the language itself but ships no
// tokenizer (its README FAQ) — without this import the island renders
// unhighlighted.
import 'monaco-editor/esm/vs/basic-languages/yaml/yaml.contribution'
import { configureMonacoYaml } from 'monaco-yaml'

const BASE = '/static/vendor/code-island/'
let version = ''

function workerUrl(name) {
  return new URL(BASE + name + '?v=' + version, document.baseURI)
}

function applyTheme(palette) {
  const isDark = palette.dark
  monaco.editor.defineTheme('harmostes', {
    base: isDark ? 'vs-dark' : 'vs',
    inherit: true,
    rules: [
      { token: 'comment', foreground: palette.muted, fontStyle: 'italic' },
      { token: 'string', foreground: palette.string },
      { token: 'keyword', foreground: palette.accent },
      { token: 'number', foreground: palette.string },
      { token: 'type', foreground: palette.accent },
    ],
    colors: {
      'editor.background': palette.bg,
      'editor.foreground': palette.fg,
      'editorLineNumber.foreground': palette.muted,
      'editorLineNumber.activeForeground': palette.fg,
      'editorGutter.background': palette.bg,
      'editor.lineHighlightBackground': palette.elevated,
      'editorWidget.background': palette.elevated,
      'editorWidget.border': palette.border,
      'editorSuggestWidget.background': palette.elevated,
      'editorSuggestWidget.border': palette.border,
      'editorSuggestWidget.selectedBackground': palette.elevatedHover,
      'editorError.foreground': palette.danger,
      'editorWarning.foreground': palette.warning,
      'scrollbarSlider.background': palette.scrollbar,
    },
  })
  monaco.editor.setTheme('harmostes')
}

// mount creates one editor instance for one document. Returns the handle
// the glue (and the Playwright tier) drives — the same API the future
// MR-bridge will use to load draft text, so nothing here is test-only.
export function mount(opts) {
  version = opts.assetVersion || ''
  globalThis.MonacoEnvironment = {
    getWorker(_moduleId, label) {
      if (label === 'yaml') return new Worker(workerUrl('yaml.worker.js'))
      return new Worker(workerUrl('editor.worker.js'))
    },
  }

  applyTheme(opts.palette)

  const uri = monaco.Uri.parse('file:///' + (opts.modelPath || 'document.yaml'))
  const model = monaco.editor.createModel(opts.text || '', 'yaml', uri)

  let yamlApi = null
  if (opts.schema) {
    yamlApi = configureMonacoYaml(monaco, {
      enableSchemaRequest: false,
      completion: true,
      hover: true,
      validate: true,
      format: true,
      schemas: [{ uri: opts.schemaUri, fileMatch: ['*'], schema: opts.schema }],
    })
  }

  const editor = monaco.editor.create(opts.el, {
    model,
    readOnly: !!opts.readOnly,
    automaticLayout: true,
    fontFamily: opts.palette.mono,
    fontSize: opts.palette.fontSize,
    minimap: { enabled: false },
    scrollBeyondLastLine: false,
    renderLineHighlight: 'line',
    wordBasedSuggestions: 'off',
    scrollbar: { verticalScrollbarSize: 8, horizontalScrollbarSize: 8 },
    padding: { top: 10, bottom: 10 },
  })

  const changeSub = model.onDidChangeContent(() => {
    if (handle.onChange) handle.onChange(model.getValue())
  })

  const handle = {
    onChange: null,
    getText: () => model.getValue(),
    // The MR-bridge / e2e path: load different text into the same island.
    setText: (v) => model.setValue(v),
    // Re-arm the schema (the /api/schema ETag changed under us).
    reconfigure: (schema) => {
      if (yamlApi) {
        yamlApi.reconfigure({
          enableSchemaRequest: false,
          completion: true,
          hover: true,
          validate: true,
          format: true,
          schemas: [{ uri: opts.schemaUri, fileMatch: ['*'], schema }],
        })
      }
    },
    setTheme: applyTheme,
    // Markers of severity Warning and up, the island's validation verdict.
    markers: () =>
      monaco.editor
        .getModelMarkers({ resource: uri })
        .filter((m) => m.severity >= 4),
    focus: () => editor.focus(),
    // Trigger the completion popover programmatically — positioned at the
    // document end (the natural "what can I write here" spot). This is the
    // MR-bridge surface (#418): a bridge loads draft text and completes
    // against the schema. Monaco suppresses the suggest controller while
    // readOnly holds, so the programmatic path lifts it for the trigger
    // and restores immediately — the user-facing document stays read-only.
    triggerSuggest: () => {
      const was = editor.getOption(monaco.editor.EditorOption.readOnly)
      if (was) editor.updateOptions({ readOnly: false })
      const last = model.getLineCount()
      editor.setPosition({ lineNumber: last, column: model.getLineMaxColumn(last) })
      editor.focus()
      editor.trigger('harmostes', 'editor.action.triggerSuggest', {})
      if (was) editor.updateOptions({ readOnly: true })
    },
    dispose: () => {
      changeSub.dispose()
      editor.dispose()
      model.dispose()
      if (yamlApi) yamlApi.dispose()
    },
  }
  return handle
}
