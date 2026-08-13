import { useEffect, useState } from 'react'
import type { SourcesJSON } from '../../shared/cli-types'

// The desktop first-run wizard — the same choices the CLI wizard offers: a
// detected subscription (no extra cost), hosted sign-in, your own API key, a
// custom endpoint, or skip. The CLI owns detection (config sources) and storage
// (config set / login); this is just the front door.
export function SetupWizard(props: { onDone: () => void }) {
  const [sources, setSources] = useState<SourcesJSON | null>(null)
  const [busy, setBusy] = useState<string | null>(null)
  const [err, setErr] = useState('')
  const [pane, setPane] = useState<'menu' | 'key' | 'endpoint'>('menu')
  const [keyVal, setKeyVal] = useState('')
  const [endpointVal, setEndpointVal] = useState('')

  useEffect(() => {
    window.memcode.sources().then(setSources).catch(() => setSources(null))
  }, [])

  const run = async (label: string, fn: () => Promise<unknown>) => {
    setBusy(label)
    setErr('')
    try {
      await fn()
      props.onDone()
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e))
    } finally {
      setBusy(null)
    }
  }

  const useSubscription = (id: string) => run(id, () => window.memcode.setConfig({ MEMCODE_CREDENTIAL_SOURCE: id }))
  const signIn = () => run('login', () => window.memcode.login())
  const saveKey = () => {
    const v = keyVal.trim()
    if (!v) return
    const env = v.startsWith('sk-ant-') ? 'ANTHROPIC_API_KEY' : 'OPENAI_API_KEY'
    return run('key', () => window.memcode.setConfig({ [env]: v }))
  }
  const saveEndpoint = () => {
    const v = endpointVal.trim()
    if (!v) return
    return run('endpoint', () => window.memcode.setConfig({ MEMCODE_ENDPOINT_URL: v }))
  }

  return (
    <div className="overlay">
      <div className="dialog wizard">
        <h2>Welcome to memcode</h2>
        <p className="detail">Pick how you'd like to run it — you can change this anytime in Settings.</p>

        {pane === 'menu' && (
          <div className="wizard-options">
            {sources?.subscriptions.map((s) => (
              <button key={s.id} className="wizard-opt recommended" disabled={!!busy} onClick={() => useSubscription(s.id)}>
                <span className="wo-title">Use your {s.label}</span>
                <span className="wo-sub">no extra cost {busy === s.id ? '· saving…' : ''}</span>
              </button>
            ))}
            <button className="wizard-opt" disabled={!!busy} onClick={signIn}>
              <span className="wo-title">Sign in to memcode</span>
              <span className="wo-sub">hosted — metered, no API keys {busy === 'login' ? '· waiting for browser…' : ''}</span>
            </button>
            <button className="wizard-opt" disabled={!!busy} onClick={() => setPane('key')}>
              <span className="wo-title">Use your own API key</span>
              <span className="wo-sub">Anthropic or OpenAI</span>
            </button>
            <button className="wizard-opt" disabled={!!busy} onClick={() => setPane('endpoint')}>
              <span className="wo-title">Point at a custom endpoint</span>
              <span className="wo-sub">Ollama, vLLM, a provider URL</span>
            </button>
            <button className="wizard-skip" disabled={!!busy} onClick={props.onDone}>
              Skip for now
            </button>
          </div>
        )}

        {pane === 'key' && (
          <div className="wizard-pane">
            <label>Paste your API key</label>
            <input
              className="cmd"
              type="password"
              placeholder="sk-ant-… (Anthropic) or sk-… (OpenAI)"
              value={keyVal}
              autoFocus
              onChange={(e) => setKeyVal(e.target.value)}
            />
            <p className="hint">Stored locally by the CLI in ~/.config/memcode/.env. Never sent anywhere but the provider.</p>
            <div className="dialog-actions">
              <button className="ghost" onClick={() => setPane('menu')}>
                Back
              </button>
              <button className="primary" disabled={!keyVal.trim() || !!busy} onClick={saveKey}>
                Save key
              </button>
            </div>
          </div>
        )}

        {pane === 'endpoint' && (
          <div className="wizard-pane">
            <label>Endpoint base URL</label>
            <input
              className="cmd"
              placeholder="http://localhost:11434/v1"
              value={endpointVal}
              autoFocus
              onChange={(e) => setEndpointVal(e.target.value)}
            />
            <div className="dialog-actions">
              <button className="ghost" onClick={() => setPane('menu')}>
                Back
              </button>
              <button className="primary" disabled={!endpointVal.trim() || !!busy} onClick={saveEndpoint}>
                Save endpoint
              </button>
            </div>
          </div>
        )}

        {err && <div className="wizard-err">{err}</div>}
      </div>
    </div>
  )
}
