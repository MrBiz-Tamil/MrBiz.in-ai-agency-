import { describe, expect, it } from 'vitest';
import { simulatedSystems } from './simulated';

const env = (m: Record<string, string>) => (k: string) => m[k];

describe('simulatedSystems', () => {
	it('lists everything when the default is fake', () => {
		expect(simulatedSystems(env({ VENTURE_CONNECTORS: 'fake' }))).toHaveLength(5);
		expect(simulatedSystems(env({}))).toHaveLength(5); // unset is treated as simulated: the safe assumption to show
	});

	it('lists only the systems that are not real when the default is real', () => {
		const sim = simulatedSystems(env({ VENTURE_CONNECTORS: 'real', VENTURE_CONNECTOR_CRM: 'fake', VENTURE_CONNECTOR_SUPPORT: 'fake' }));
		expect(sim).toEqual(['CRM (built-in)', 'support (LibreDesk)']);
	});

	it('lets a per-system override make one system real', () => {
		const sim = simulatedSystems(env({ VENTURE_CONNECTORS: 'fake', VENTURE_CONNECTOR_ERP: 'real' }));
		expect(sim).toHaveLength(4);
		expect(sim).not.toContain('ERP (ERPNext)');
	});

	it('shows nothing when every system is real', () => {
		expect(simulatedSystems(env({ VENTURE_CONNECTORS: 'real' }))).toEqual([]);
	});

	it('ignores case and whitespace', () => {
		expect(simulatedSystems(env({ VENTURE_CONNECTORS: ' REAL ', VENTURE_CONNECTOR_CRM: ' Fake ' }))).toEqual(['CRM (built-in)']);
	});
});
