<script lang="ts">
	import { Bot, Users } from 'lucide-svelte';
	import { getContextClient } from '@urql/svelte';
	import { page } from '$app/stores';
	import { onMount } from 'svelte';
	import { Q_ROSTER, M_BUDGET, M_PAUSE, M_RESUME, M_HEARTBEAT, errorMessage, type Agent } from '$lib/graphql/operations';

	const client = getContextClient();
	let workspaceId = $derived($page.data.session!.workspaceId);
	let role = $derived($page.data.session!.role);
	let canOperate = $derived(role === 'owner' || role === 'admin' || role === 'operator');
	let canAdmin = $derived(role === 'owner' || role === 'admin');

	let agents = $state<Agent[]>([]);
	let loading = $state(true);
	let loadError = $state<string | null>(null);

	let selectedId = $state<string | null>(null);
	let selected = $derived(agents.find((a) => a.id === selectedId) ?? null);
	let tab = $state<'identity' | 'heartbeats' | 'guardrails'>('identity');
	let budgetInput = $state('');
	let message = $state<{ kind: 'ok' | 'error'; text: string } | null>(null);
	let busy = $state(false);

	async function load() {
		loading = true;
		const res = await client.query(Q_ROSTER, { ws: workspaceId }, { requestPolicy: 'network-only' }).toPromise();
		loading = false;
		if (res.error) loadError = errorMessage(res.error);
		else {
			loadError = null;
			agents = res.data?.agentRoster ?? [];
		}
	}
	onMount(load);

	function open(a: Agent) {
		selectedId = a.id;
		tab = 'identity';
		budgetInput = a.dailyBudgetUsd;
		message = null;
	}
	function close() {
		selectedId = null;
	}

	function replace(a: Agent) {
		const i = agents.findIndex((x) => x.id === a.id);
		if (i !== -1) agents[i] = a;
	}

	async function run(fn: () => Promise<{ data?: any; error?: any }>, apply: (data: any) => void, okText: string) {
		if (busy) return;
		busy = true;
		message = null;
		const res = await fn();
		busy = false;
		if (res.error) message = { kind: 'error', text: errorMessage(res.error) ?? 'Failed.' };
		else {
			apply(res.data);
			message = { kind: 'ok', text: okText };
		}
	}

	const saveBudget = () => {
		const id = selected!.id;
		return run(
			() => client.mutation(M_BUDGET, { id, usd: budgetInput.trim() }).toPromise(),
			(d) => replace(d.updateAgentBudget),
			'Budget updated.'
		);
	};
	const pause = () => run(() => client.mutation(M_PAUSE, { id: selected!.id }).toPromise(), (d) => replace(d.pauseAgent), 'Agent paused.');
	const resume = () => run(() => client.mutation(M_RESUME, { id: selected!.id }).toPromise(), (d) => replace(d.resumeAgent), 'Agent resumed.');
	const heartbeat = () => run(() => client.mutation(M_HEARTBEAT, { id: selected!.id }).toPromise(), () => {}, 'Heartbeat completed.');

	function pct(a: Agent): number {
		const spent = parseFloat(a.spentTodayUsd);
		const budget = parseFloat(a.dailyBudgetUsd);
		if (!(budget > 0)) return spent > 0 ? 100 : 0;
		return Math.max(0, Math.min(100, (spent / budget) * 100));
	}

	function onKeydown(e: KeyboardEvent) {
		if (e.key === 'Escape' && selected) close();
	}
	function onBackdrop(e: MouseEvent) {
		if (e.target === e.currentTarget) close();
	}

	const tabs = [['identity', 'Identity'], ['heartbeats', 'Heartbeats'], ['guardrails', 'Guardrails']] as const;
</script>

<svelte:head><title>Agent Org Chart | ABOS</title></svelte:head>
<svelte:window onkeydown={onKeydown} />

<div class="flex-1 p-6 flex flex-col overflow-y-auto">
	<div class="mb-6">
		<h2 class="text-xl font-bold text-slate-100 flex items-center gap-2">
			<Users class="w-5 h-5 text-indigo-400" />
			<span>Specialist Agent Workforce ({agents.length} roles)</span>
		</h2>
		<p class="text-xs text-slate-400 mt-1">Agents run scheduled heartbeats and event-driven workflows under hard budget circuit breakers.</p>
	</div>

	{#if loadError}
		<p class="text-xs text-rose-300 mb-4" role="alert">Could not load agents: {loadError} <button class="underline ml-2" onclick={load}>Retry</button></p>
	{/if}
	{#if loading}
		<p class="text-xs text-slate-500 font-mono">Loading…</p>
	{:else if agents.length === 0 && !loadError}
		<p class="text-xs text-slate-500 font-mono">No agents are configured for this workspace.</p>
	{/if}

	<div class="grid grid-cols-4 gap-6">
		{#each agents as agent (agent.id)}
			<button type="button" class="text-left bg-surface-900 border border-surface-800 rounded-2xl p-5 hover:border-indigo-500/50 transition-all flex flex-col justify-between" onclick={() => open(agent)}>
				<div>
					<div class="flex justify-between items-start mb-3">
						<div class="w-10 h-10 rounded-xl bg-surface-950 border border-surface-800 flex items-center justify-center text-indigo-400"><Bot class="w-5 h-5" /></div>
						<div class="flex flex-col items-end gap-1">
							<span class="text-[10px] font-mono font-semibold px-2 py-0.5 rounded {agent.modelTier === 'FRONTIER' ? 'bg-purple-950/80 text-purple-300 border border-purple-800/80' : 'bg-blue-950/80 text-blue-300 border border-blue-800/80'}">{agent.modelTier}</span>
							{#if agent.status !== 'ACTIVE'}<span class="text-[10px] font-mono px-2 py-0.5 rounded bg-amber-900 text-amber-200">{agent.status}</span>{/if}
						</div>
					</div>
					<h3 class="text-sm font-bold text-slate-100 leading-snug">{agent.roleName}</h3>
				</div>
				<div class="mt-6 pt-4 border-t border-surface-800 w-full">
					<div class="flex justify-between items-center text-xs mb-1.5 font-mono">
						<span class="text-slate-400 text-[10px]">Daily Budget</span>
						<span class="text-slate-200 font-semibold">${parseFloat(agent.spentTodayUsd).toFixed(2)} / ${parseFloat(agent.dailyBudgetUsd).toFixed(2)}</span>
					</div>
					<div class="w-full h-1.5 rounded-full bg-surface-950 overflow-hidden" role="progressbar" aria-label="Budget used" aria-valuemin="0" aria-valuemax="100" aria-valuenow={Math.round(pct(agent))}>
						<div class="h-full rounded-full transition-all {pct(agent) >= 100 ? 'bg-rose-500' : pct(agent) > 80 ? 'bg-amber-400' : 'bg-indigo-500'}" style="width: {pct(agent)}%"></div>
					</div>
					<div class="mt-3 flex justify-between items-center text-[10px] text-slate-500 font-mono">
						<span>{agent.heartbeatCron ? `Cron: ${agent.heartbeatCron}` : 'Event-driven'}</span>
						<span class="text-indigo-400">Manage &rarr;</span>
					</div>
				</div>
			</button>
		{/each}
	</div>
</div>

{#if selected}
	<div class="fixed inset-0 bg-black/60 backdrop-blur-sm z-[60] flex justify-end" role="presentation" onclick={onBackdrop}>
		<div class="w-full max-w-lg bg-surface-900 border-l border-surface-800 h-full p-6 flex flex-col justify-between overflow-y-auto" role="dialog" aria-modal="true" aria-labelledby="agent-title">
			<div>
				<div class="flex justify-between items-start pb-4 border-b border-surface-800">
					<div>
						<span class="text-xs text-indigo-400 font-mono font-semibold">{selected.modelTier} TIER · {selected.status}</span>
						<h2 id="agent-title" class="text-lg font-bold text-slate-100 mt-1">{selected.roleName}</h2>
					</div>
					<button type="button" class="text-slate-400 hover:text-slate-200 text-xs font-mono px-2 py-1 bg-surface-800 rounded" onclick={close}>ESC</button>
				</div>

				<div class="flex border-b border-surface-800 mt-4 text-xs font-medium text-slate-400" role="tablist">
					{#each tabs as [key, label]}
						<button type="button" role="tab" aria-selected={tab === key}
							class="pb-2.5 px-3 border-b-2 {tab === key ? 'border-indigo-500 text-indigo-400' : 'border-transparent hover:text-slate-200'}"
							onclick={() => (tab = key)}>{label}</button>
					{/each}
				</div>

				<div class="py-6 text-xs text-slate-300 space-y-4">
					{#if tab === 'identity'}
						<dl class="grid grid-cols-2 gap-3">
							<div class="p-3 bg-surface-950 rounded-xl border border-surface-800"><dt class="text-slate-400 font-mono text-[10px] uppercase">Model tier</dt><dd class="mt-1 text-slate-200">{selected.modelTier}</dd></div>
							<div class="p-3 bg-surface-950 rounded-xl border border-surface-800"><dt class="text-slate-400 font-mono text-[10px] uppercase">Status</dt><dd class="mt-1 text-slate-200">{selected.status}</dd></div>
						</dl>
						<div class="flex gap-2">
							{#if selected.status === 'ACTIVE'}
								<button type="button" disabled={!canOperate || busy} onclick={pause}
									class="px-3 py-2 rounded-lg bg-surface-800 hover:bg-surface-700 text-slate-200 font-semibold disabled:opacity-50">Pause agent</button>
							{:else}
								<button type="button" disabled={!canOperate || busy} onclick={resume}
									class="px-3 py-2 rounded-lg bg-emerald-700 hover:bg-emerald-600 text-white font-semibold disabled:opacity-50">Resume agent</button>
							{/if}
						</div>
						{#if !canOperate}<p class="text-slate-500">Your role can view agents but not change them.</p>{/if}
					{:else if tab === 'heartbeats'}
						<div class="p-4 bg-surface-950 rounded-xl border border-surface-800">
							<span class="text-slate-400 font-mono text-[10px] uppercase block mb-1">Proactive schedule</span>
							<code class="text-slate-200">{selected.heartbeatCron ?? 'Event-driven (no schedule)'}</code>
						</div>
						<button type="button" disabled={!canOperate || busy || selected.status !== 'ACTIVE'} onclick={heartbeat}
							class="px-3 py-2 rounded-lg bg-indigo-600 hover:bg-indigo-500 text-white font-semibold disabled:opacity-50">Run heartbeat now</button>
					{:else}
						<div class="p-4 bg-surface-950 rounded-xl border border-surface-800">
							<label for="agent-budget" class="text-slate-400 font-mono text-[10px] uppercase block mb-1">Daily hard budget cap (USD)</label>
							<input id="agent-budget" type="number" min="0" step="0.01" bind:value={budgetInput} disabled={!canAdmin}
								class="w-full bg-surface-900 border border-surface-800 rounded-lg p-2 text-xs font-mono text-slate-200 disabled:opacity-60" />
							<span class="text-[10px] text-amber-400 mt-2 block">The circuit breaker trips at 100% and rejects further spend. Alerts fire at 80%.</span>
						</div>
						<button type="button" disabled={!canAdmin || busy} onclick={saveBudget}
							class="px-3 py-2 rounded-lg bg-indigo-600 hover:bg-indigo-500 text-white font-semibold disabled:opacity-50">Save budget</button>
						{#if !canAdmin}<p class="text-slate-500">Only owners and admins can change budgets.</p>{/if}
					{/if}
				</div>
			</div>

			<div class="pt-4 border-t border-surface-800 min-h-[2rem]">
				{#if message}<p class="text-xs {message.kind === 'ok' ? 'text-emerald-400' : 'text-rose-400'}" role={message.kind === 'ok' ? 'status' : 'alert'}>{message.text}</p>{/if}
			</div>
		</div>
	</div>
{/if}
