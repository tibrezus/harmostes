// Monaco's base editor worker: serves the editorWorkerService requests that
// are not language-specific. The island disables word-based suggestions, but
// Monaco still routes some requests here; serving them keeps the editor
// fully functional instead of silently degraded.
import 'monaco-editor/esm/vs/editor/editor.worker'
