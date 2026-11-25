<script lang="ts">
	import type { StepView } from '$lib/types';

	let { steps, running = false }: { steps: StepView[]; running?: boolean } = $props();

	const LABELS: Record<string, string> = {
		'identity.organization': 'Create the organization in identity & access',
		'db.seed': 'Set up the books, pricing rules, pipelines and workflows',
		'erp.company': 'Create the company and chart of accounts in the ERP',
		'crm.workspace': 'Create the CRM workspace',
		'support.inboxes': 'Create the support inboxes',
		'runtime.profile': 'Create the agents’ runtime profile',
		'crm.pipelines': 'Configure the CRM pipelines',
		'support.kb': 'Seed the knowledge base',
		'runtime.skills': 'Install the agents’ skills',
		'tenant.activate': 'Go live'
	};
	const ORDER = Object.keys(LABELS);
	const rows = $derived(ORDER.map((key) => steps.find((s) => s.key === key) ?? { key, status: running ? 'PENDING' : 'PENDING', attempts: 0, error: '' }));
	const icon = (s: string) => ({ SUCCEEDED: '✓', RUNNING: '…', FAILED: '✕', COMPENSATED: '↺', COMPENSATION_FAILED: '!' })[s] ?? '○';
	const tone = (s: string) => (s === 'SUCCEEDED' ? 'text-good' : s === 'FAILED' || s === 'COMPENSATION_FAILED' ? 'text-bad' : s === 'RUNNING' ? 'text-accent' : 'text-ink-3');
</script>

<ol class="space-y-1.5" aria-label="Launch progress">
	{#each rows as s (s.key)}
		<li class="flex items-start gap-3 text-sm">
			<span class="mt-0.5 grid size-5 shrink-0 place-items-center rounded-full border border-current text-xs {tone(s.status)} {s.status === 'RUNNING' ? 'animate-pulse' : ''}" aria-hidden="true">{icon(s.status)}</span>
			<div>
				<span class={s.status === 'PENDING' ? 'text-ink-3' : 'text-ink'}>{LABELS[s.key]}</span>
				{#if s.status === 'COMPENSATED'}<span class="ml-2 text-xs text-ink-3">rolled back</span>{/if}
				{#if s.attempts > 1}<span class="ml-2 text-xs text-ink-3">{s.attempts} attempts</span>{/if}
				{#if s.error && (s.status === 'FAILED' || s.status === 'COMPENSATION_FAILED')}<p class="text-xs text-bad">{s.error}</p>{/if}
			</div>
			<span class="sr-only">{s.status.toLowerCase()}</span>
		</li>
	{/each}
</ol>
