// Which external systems the control plane simulates, derived from the same variables it reads
// (VENTURE_CONNECTORS and VENTURE_CONNECTOR_<SYSTEM>). Used only to tell the user the truth in the UI.

export const SYSTEMS: { key: string; label: string }[] = [
	{ key: 'IDENTITY', label: 'identity (Keycloak)' },
	{ key: 'ERP', label: 'ERP (ERPNext)' },
	{ key: 'CRM', label: 'CRM (built-in)' },
	{ key: 'SUPPORT', label: 'support (LibreDesk)' },
	{ key: 'RUNTIME', label: 'agent runtime (Hermes)' }
];

/** Labels of the systems that are simulated under the given environment. */
export function simulatedSystems(get: (name: string) => string | undefined): string[] {
	const base = (get('VENTURE_CONNECTORS') ?? 'fake').trim().toLowerCase();
	return SYSTEMS.filter((s) => {
		const override = (get(`VENTURE_CONNECTOR_${s.key}`) ?? '').trim().toLowerCase();
		return (override || base) !== 'real';
	}).map((s) => s.label);
}
