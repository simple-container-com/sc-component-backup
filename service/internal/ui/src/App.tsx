import { Tenants } from './pages/Tenants'

export function App() {
  return (
    <main style={{ fontFamily: 'system-ui, sans-serif', maxWidth: 900, margin: '2rem auto', padding: '0 1rem' }}>
      <header style={{ borderBottom: '1px solid #ddd', paddingBottom: '0.5rem', marginBottom: '1.5rem' }}>
        <h1 style={{ margin: 0 }}>sc-component-backup</h1>
        <p style={{ color: '#666', margin: '0.25rem 0 0' }}>
          Throwaway/template component for the sc-components-platform milestone.
        </p>
      </header>
      <Tenants />
    </main>
  )
}
