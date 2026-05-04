import { useEffect, useRef } from 'react';
// Import only the editor core + JSON contribution so the lazy chunk doesn't
// drag in every language Monaco ships (Razor, ABAP, COBOL, Solidity, …).
// JSON is the only syntax this editor renders today; CORS / Policy / Managed
// Policy editors all reuse this component for JSON-only payloads.
import * as monaco from 'monaco-editor/esm/vs/editor/editor.api';
import 'monaco-editor/esm/vs/language/json/monaco.contribution';
import editorWorker from 'monaco-editor/esm/vs/editor/editor.worker?worker';
import jsonWorker from 'monaco-editor/esm/vs/language/json/json.worker?worker';

// MonacoJsonEditor is the heavyweight JSON editor used by every admin tab
// that needs schema-validated JSON (Lifecycle US-004, CORS US-005, Policy
// US-006, ManagedPolicy US-013). The whole module — including the
// `monaco-editor` import — is split into its own chunk because consumers
// must import it via React.lazy(() => import('./MonacoJsonEditor')); see the
// Lifecycle tab for the canonical wiring. Workers are bundled too via the
// `?worker` suffix so the editor works without a CDN.
//
// schemaUri is the Monaco-internal URI used to bind a vendored JSON schema
// to documents whose model URI matches `schemaModelUri`. Pass undefined to
// skip schema validation.

type SchemaSpec = {
  uri: string;
  schema: object;
  modelUri: string;
};

interface Props {
  value: string;
  onChange: (next: string) => void;
  schema?: SchemaSpec;
  height?: number;
  readOnly?: boolean;
}

let workerEnvWired = false;

function ensureWorkerEnv() {
  if (workerEnvWired) return;
  workerEnvWired = true;
  // Self-hosted workers — avoids any CDN fetch so the console works offline.
  // Vite emits each worker into its own asset; the constructor is the URL.
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  (self as any).MonacoEnvironment = {
    getWorker(_workerId: string, label: string) {
      if (label === 'json') return new jsonWorker();
      return new editorWorker();
    },
  };
}

export function MonacoJsonEditor({
  value,
  onChange,
  schema,
  height = 420,
  readOnly = false,
}: Props) {
  const containerRef = useRef<HTMLDivElement | null>(null);
  const editorRef = useRef<monaco.editor.IStandaloneCodeEditor | null>(null);
  const onChangeRef = useRef(onChange);
  onChangeRef.current = onChange;

  useEffect(() => {
    ensureWorkerEnv();
    if (!containerRef.current) return;
    if (schema) {
      const existing =
        monaco.languages.json.jsonDefaults.diagnosticsOptions.schemas ?? [];
      const next = existing.filter((s) => s.uri !== schema.uri);
      next.push({
        uri: schema.uri,
        fileMatch: [schema.modelUri],
        schema: schema.schema,
      });
      monaco.languages.json.jsonDefaults.setDiagnosticsOptions({
        validate: true,
        allowComments: false,
        schemas: next,
      });
    }
    const modelUri = monaco.Uri.parse(schema?.modelUri ?? `inmemory://model/${Date.now()}.json`);
    let model = monaco.editor.getModel(modelUri);
    if (!model) {
      model = monaco.editor.createModel(value, 'json', modelUri);
    } else {
      model.setValue(value);
    }
    const editor = monaco.editor.create(containerRef.current, {
      model,
      automaticLayout: true,
      minimap: { enabled: false },
      scrollBeyondLastLine: false,
      tabSize: 2,
      readOnly,
      fontSize: 13,
    });
    editorRef.current = editor;
    const sub = editor.onDidChangeModelContent(() => {
      onChangeRef.current(editor.getValue());
    });
    return () => {
      sub.dispose();
      editor.dispose();
      // Dispose the model so the next mount with a fresh value is clean.
      model?.dispose();
    };
    // The editor instance is created once; subsequent value changes flow
    // through the explicit setValue effect below. Schema is captured at
    // mount and re-registered if the consumer remounts the component.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  // Keep the model in sync with parent-driven value changes (e.g. Visual
  // tab edits reserialise to JSON). Skip if Monaco already has the value
  // to avoid resetting cursor position on every keystroke.
  useEffect(() => {
    const editor = editorRef.current;
    if (!editor) return;
    if (editor.getValue() !== value) editor.setValue(value);
  }, [value]);

  return (
    <div
      ref={containerRef}
      className="rounded-md border"
      style={{ height: `${height}px` }}
    />
  );
}

export default MonacoJsonEditor;
