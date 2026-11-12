import { useQuery } from '@tanstack/react-query'
import axios from 'axios'

interface TenantRecord {
  stackName: string
  version: string
  resources?: Array<{ type: string; name: string }>
  createdAt: string
  lastSeenAt: string
  softDeleted?: boolean
}

interface TenantsResponse {
  tenants: TenantRecord[]
  count: number
}

async function fetchTenants(): Promise<TenantsResponse> {
  // The SPA is served from the same origin as the API. The JWT in
  // localStorage.dummyToken is produced by an operator tool; for the
  // smoke, we just show what the backend has.
  const token = localStorage.getItem('dummyToken') ?? ''
  const { data } = await axios.get<TenantsResponse>('/api/v1/tenants', {
    headers: token ? { Authorization: `Bearer ${token}` } : {},
  })
  return data
}

export function Tenants() {
  const { data, isLoading, error } = useQuery({
    queryKey: ['tenants'],
    queryFn: fetchTenants,
    refetchInterval: 5000,
  })

  if (isLoading) return <p>Loading…</p>
  if (error) {
    return (
      <div>
        <p style={{ color: '#c00' }}>Error fetching tenants: {(error as Error).message}</p>
        <p>
          Hint: set <code>localStorage.dummyToken</code> to a valid JWT (op=register, signed with the
          deployed <code>SERVICE_JWT_SECRET</code>).
        </p>
      </div>
    )
  }

  const tenants = data?.tenants ?? []
  if (tenants.length === 0) {
    return (
      <div>
        <h2>Tenants</h2>
        <p style={{ color: '#666' }}>No tenants registered yet.</p>
        <p style={{ fontSize: '0.9em', color: '#888' }}>
          A tenant appears when a consumer service deploys with <code>uses: [my-dummy]</code> in
          its <code>client.yaml</code>. The plugin's <code>ComputeProcess</code> handler pushes the
          registration to <code>POST /api/v1/tenants/register</code>.
        </p>
      </div>
    )
  }

  return (
    <div>
      <h2>Tenants ({data!.count})</h2>
      <table style={{ width: '100%', borderCollapse: 'collapse' }}>
        <thead>
          <tr style={{ borderBottom: '2px solid #333' }}>
            <th style={{ textAlign: 'left', padding: '0.5rem' }}>Stack</th>
            <th style={{ textAlign: 'left', padding: '0.5rem' }}>Version</th>
            <th style={{ textAlign: 'left', padding: '0.5rem' }}>Resources</th>
            <th style={{ textAlign: 'left', padding: '0.5rem' }}>Last seen</th>
            <th style={{ textAlign: 'left', padding: '0.5rem' }}>State</th>
          </tr>
        </thead>
        <tbody>
          {tenants.map((t) => (
            <tr key={t.stackName} style={{ borderBottom: '1px solid #eee' }}>
              <td style={{ padding: '0.5rem', fontFamily: 'monospace' }}>{t.stackName}</td>
              <td style={{ padding: '0.5rem' }}>{t.version}</td>
              <td style={{ padding: '0.5rem' }}>{t.resources?.length ?? 0}</td>
              <td style={{ padding: '0.5rem', fontFamily: 'monospace', fontSize: '0.9em' }}>
                {new Date(t.lastSeenAt).toISOString()}
              </td>
              <td style={{ padding: '0.5rem' }}>{t.softDeleted ? 'deleted' : 'active'}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  )
}
