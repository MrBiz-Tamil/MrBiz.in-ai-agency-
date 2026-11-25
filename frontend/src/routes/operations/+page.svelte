<script lang="ts">
	import { AlertTriangle, CheckCircle2, Clock, Eye, ShieldCheck, WifiOff } from 'lucide-svelte';
	import { getContextClient } from '@urql/svelte';
	import { page } from '$app/stores';
	import { onMount, onDestroy } from 'svelte';
	import ApprovalDrawer from '$lib/components/ApprovalDrawer.svelte';
	import DecisionXRayModal from '$lib/components/DecisionXRayModal.svelte';
	import {
		Q_TICKETS, S_TICKET_UPDATED, M_APPROVE, M_REJECT, Q_ROSTER, errorMessage, resolution,
		type Ticket, type Agent
	} from '$lib/graphql/operations';

	const client = getContextClient();
	let workspaceId = $derived($page.data.session!.workspaceId);
	let role = $derived($page.data.session!.role);
	let canDecide = $derived(role === 'owner' || role === 'admin' || role === 'operator');

	let tickets = $state<Ticket[]>([]);
	let agents = $state<Agent[]>([]);
	let loading = $state(true);
	let loadError = $state<string | null>(null);
	let live = $state<'connecting' | 'live' | 'down'>('connecting');
	let unsubscribe: (() => void) | undefined;

	function upsert(t: Ticket) {
		const i = tickets.findIndex((x) => x.id === t.id);
		if (i === -1) tickets = [t, ...tickets];
		else tickets[i] = t;
	}

	async function load() {
		loading = true;
		const [res, roster] = await Promise.all([
			client.query(Q_TICKETS, { ws: workspaceId }, { requestPolicy: 'network-only' }).toPromise(),
			client.query(Q_ROSTER, { ws: workspaceId }, { requestPolicy: 'network-only' }).toPromise()
		]);
		loading = false;
		if (res.error) loadError = errorMessage(res.error);
		else {
			loadError = null;
			tickets = res.data?.tickets ?? [];
		}
		agents = roster.data?.agentRoster ?? [];
	}

	onMount(async () => {
		await load();
		const sub = client.subscription(S_TICKET_UPDATED, { ws: workspaceId }).subscribe((r) => {
			if (r.error) {
				live = 'down';
				return;
			}
			live = 'live';
			if (r.data?.ticketUpdated) upsert(r.data.ticketUpdated);
		});
		unsubscribe = () => sub.unsubscribe();
		// If nothing arrives quickly, show that live updates are not (yet) connected.
		setTimeout(() => {
			if (live === 'connecting') live = 'live'; // lazily-connected sockets report no event until the first change
		}, 4000);
	});
	onDestroy(() => unsubscribe?.());

	let activeIngestion = $derived(tickets.filter((t) => t.status === 'QUEUED' || t.status === 'RUNNING'));
	let pendingApprovals = $derived(
		tickets.filter((t) => t.status === 'PENDING_APPROVAL' || t.status === 'CONFLICT_HOLD').sort((a, b) => a.priority - b.priority)
	);
	let closedTickets = $derived(tickets.filter((t) => t.status === 'COMPLETED' || t.status === 'FAILED'));

	// Touchless rate over the loaded tickets: completed without any human decision.
	let touchlessRate = $derived.by(() => {
		if (closedTickets.length === 0) return null;
		const n = closedTickets.filter((t) => resolution(t) === 'touchless').length;
		return Math.round((n / closedTickets.length) * 1000) / 10;
	});

	let spend = $derived.by(() => {
		if (agents.length === 0) return null;
		const spent = agents.reduce((s, a) => s + parseFloat(a.spentTodayUsd), 0);
		const budget = agents.reduce((s, a) => s + parseFloat(a.dailyBudgetUsd), 0);
		return { spent, budget };
	});

	let drawerId = $state<string | null>(null);
	let xrayId = $state<string | null>(null);
	let drawerTicket = $derived(tickets.find((t) => t.id === drawerId) ?? null);
	let xrayTicket = $derived(tickets.find((t) => t.id === xrayId) ?? null);

	async function approve(id: string, reason: string): Promise<string | null> {
		const res = await client.mutation(M_APPROVE, { input: { ticketId: id, overrideReason: reason || null } }).toPromise();
		if (res.error) return errorMessage(res.error);
		if (res.data?.approveTicket) upsert(res.data.approveTicket);
		return null;
	}

	async function reject(id: string, reason: string): Promise<string | null> {
		const res = await client.mutation(M_REJECT, { input: { ticketId: id, rejectionReason: reason } }).toPromise();
		if (res.error) return errorMessage(res.error);
		if (res.data?.rejectTicket) upsert(res.data.rejectTicket);
		return null;
	}

	function resolutionLabel(t: Ticket): string {
		switch (resolution(t)) {
			case 'human_approved': return '✓ Approved by human';
			case 'rejected': return '✕ Rejected';
			case 'failed': return '✕ Failed';
			default: return '✓ Touchless';
		}
	}
</script>

<svelte:head><title>Live Execution Board | ABOS</title></svelte:head>

<div class="flex-1 p-6 flex flex-col overflow-hidden">
	{#if live === 'down'}
		<div class="mb-4 flex items-center gap-2 text-xs text-amber-300 bg-amber-950/30 border border-amber-800/60 rounded-lg px-3 py-2" role="status">
			<WifiOff class="w-4 h-4" /> Live updates are disconnected; reconnecting. Reload to fetch the latest tickets.
		</div>
	{/if}
	{#if loadError}
		<div class="mb-4 text-xs text-rose-300 bg-rose-950/30 border border-rose-800/60 rounded-lg px-3 py-2" role="alert">
			Could not load tickets: {loadError}
			<button type="button" class="underline ml-2" onclick={load}>Retry</button>
		</div>
	{/if}

	<div class="grid grid-cols-4 gap-4 mb-6">
		<div class="p-4 rounded-xl bg-surface-900 border border-surface-800 flex items-center justify-between" title="Share of finished tickets (of the latest 100) that needed no human decision">
			<div>
				<span class="text-xs text-slate-400 font-mono">Touchless Resolution</span>
				<div class="text-2xl font-bold text-slate-100 mt-1">{touchlessRate === null ? '—' : `${touchlessRate}%`}</div>
			</div>
			<div class="w-10 h-10 rounded-lg bg-emerald-950/60 border border-emerald-800/80 flex items-center justify-center text-emerald-400"><CheckCircle2 class="w-5 h-5" /></div>
		</div>

		<div class="p-4 rounded-xl bg-surface-900 border border-surface-800 flex items-center justify-between" title="Not connected: voice telemetry is not implemented yet">
			<div>
				<span class="text-xs text-slate-400 font-mono">Voice P95 Latency</span>
				<div class="text-2xl font-bold text-slate-500 mt-1">—</div>
			</div>
			<div class="w-10 h-10 rounded-lg bg-indigo-950/60 border border-indigo-800/80 flex items-center justify-center text-indigo-400"><Clock class="w-5 h-5" /></div>
		</div>

		<div class="p-4 rounded-xl bg-surface-900 border border-surface-800 flex items-center justify-between">
			<div>
				<span class="text-xs text-slate-400 font-mono">Active Exceptions</span>
				<div class="text-2xl font-bold text-amber-400 mt-1">{pendingApprovals.length}</div>
			</div>
			<div class="w-10 h-10 rounded-lg bg-amber-950/60 border border-amber-800/80 flex items-center justify-center text-amber-400"><AlertTriangle class="w-5 h-5" /></div>
		</div>

		<div class="p-4 rounded-xl bg-surface-900 border border-surface-800 flex items-center justify-between" title="Sum across all agents for today (UTC)">
			<div>
				<span class="text-xs text-slate-400 font-mono">Today's Token Spend</span>
				<div class="text-2xl font-bold text-slate-100 mt-1">
					{#if spend}${spend.spent.toFixed(2)} <span class="text-xs text-slate-500 font-normal">/ ${spend.budget.toFixed(2)}</span>{:else}—{/if}
				</div>
			</div>
			<div class="w-10 h-10 rounded-lg bg-surface-800 flex items-center justify-center text-slate-400"><ShieldCheck class="w-5 h-5" /></div>
		</div>
	</div>

	<div class="grid grid-cols-3 gap-6 flex-1 overflow-hidden">
		<!-- Lane 1 -->
		<div class="bg-surface-900 border border-surface-800 rounded-2xl p-4 flex flex-col overflow-hidden">
			<div class="flex items-center justify-between pb-3 border-b border-surface-800 mb-4">
				<div class="flex items-center gap-2">
					<span class="w-2.5 h-2.5 rounded-full bg-indigo-400 animate-pulse"></span>
					<h3 class="text-xs font-bold uppercase tracking-wider text-slate-200">Processing & Ingestion ({activeIngestion.length})</h3>
				</div>
				<span class="text-[10px] font-mono {live === 'live' ? 'text-emerald-400' : 'text-slate-500'}">{live === 'live' ? '● live' : live === 'down' ? '○ offline' : '○ connecting'}</span>
			</div>
			<div class="space-y-3 overflow-y-auto flex-1 pr-1">
				{#each activeIngestion as ticket (ticket.id)}
					<div class="p-4 bg-surface-950 border border-surface-800 rounded-xl hover:border-surface-700 transition-all">
						<div class="flex justify-between items-center text-xs">
							<span class="font-mono text-[10px] text-slate-400">{ticket.assignedAgent?.roleName ?? 'Unassigned'}</span>
							<span class="text-[10px] font-mono px-2 py-0.5 rounded bg-indigo-950/60 text-indigo-300 border border-indigo-800/60">{ticket.status}</span>
						</div>
						<h4 class="text-sm font-semibold text-slate-100 mt-2 font-mono break-all">{ticket.actionType}</h4>
						<div class="mt-3 flex justify-between items-center pt-2 border-t border-surface-900 text-[10px] text-slate-500 font-mono">
							<span>{ticket.id.slice(0, 8)}</span>
							<button type="button" class="text-indigo-400 hover:text-indigo-300 flex items-center gap-1" onclick={() => (xrayId = ticket.id)}>
								<Eye class="w-3 h-3" /><span>X-Ray</span>
							</button>
						</div>
					</div>
				{:else}
					<p class="text-xs text-slate-500 font-mono text-center py-6">{loading ? 'Loading…' : 'Nothing in flight'}</p>
				{/each}
			</div>
		</div>

		<!-- Lane 2 -->
		<div class="bg-surface-900 border border-amber-900/40 rounded-2xl p-4 flex flex-col overflow-hidden">
			<div class="flex items-center justify-between pb-3 border-b border-amber-900/40 mb-4">
				<div class="flex items-center gap-2">
					<span class="w-2.5 h-2.5 rounded-full bg-amber-400"></span>
					<h3 class="text-xs font-bold uppercase tracking-wider text-amber-300">Requires Approval ({pendingApprovals.length})</h3>
				</div>
				<span class="text-[10px] font-mono text-amber-400">Action Required</span>
			</div>
			<div class="space-y-3 overflow-y-auto flex-1 pr-1">
				{#each pendingApprovals as ticket (ticket.id)}
					<button type="button" class="w-full text-left p-4 bg-amber-950/20 border border-amber-800/80 rounded-xl hover:border-amber-600 transition-all shadow-lg shadow-amber-950/10"
						onclick={() => (drawerId = ticket.id)}>
						<div class="flex justify-between items-center text-xs">
							<span class="font-mono text-[10px] text-amber-400">P{ticket.priority}</span>
							<span class="text-[10px] font-bold font-mono px-2 py-0.5 rounded bg-amber-900 text-amber-200">{ticket.status}</span>
						</div>
						<h4 class="text-sm font-semibold text-slate-100 mt-2 font-mono break-all">{ticket.actionType}</h4>
						{#if ticket.conflictDiff}
							<div class="mt-2.5 p-2 bg-surface-950/80 rounded-lg text-xs font-mono text-amber-300 border border-amber-900/40 break-words">
								{ticket.conflictDiff.field}: {ticket.conflictDiff.valueA || '—'} vs {ticket.conflictDiff.valueB}
							</div>
						{/if}
						<div class="mt-3 flex justify-between items-center pt-2 border-t border-amber-900/30 text-[10px] text-slate-400 font-mono">
							<span>Click to review</span>
							<span class="text-amber-400 font-semibold">Review &rarr;</span>
						</div>
					</button>
				{:else}
					<p class="text-xs text-slate-500 font-mono text-center py-6">{loading ? 'Loading…' : 'No exceptions'}</p>
				{/each}
			</div>
		</div>

		<!-- Lane 3 -->
		<div class="bg-surface-900 border border-surface-800 rounded-2xl p-4 flex flex-col overflow-hidden">
			<div class="flex items-center justify-between pb-3 border-b border-surface-800 mb-4">
				<div class="flex items-center gap-2">
					<span class="w-2.5 h-2.5 rounded-full bg-emerald-400"></span>
					<h3 class="text-xs font-bold uppercase tracking-wider text-emerald-400">Completed ({closedTickets.length})</h3>
				</div>
			</div>
			<div class="space-y-3 overflow-y-auto flex-1 pr-1">
				{#each closedTickets as ticket (ticket.id)}
					<div class="p-4 bg-surface-950 border border-surface-800 rounded-xl opacity-90">
						<div class="flex justify-between items-center text-xs">
							<span class="font-mono text-[10px] text-slate-400">{ticket.assignedAgent?.roleName ?? 'Unassigned'}</span>
							<span class="text-[10px] font-mono {ticket.status === 'FAILED' ? 'text-rose-400' : 'text-emerald-400'}">{resolutionLabel(ticket)}</span>
						</div>
						<h4 class="text-sm font-semibold text-slate-200 mt-2 font-mono break-all">{ticket.actionType}</h4>
						{#if ticket.status === 'FAILED' && ticket.result?.error}
							<p class="mt-2 text-[11px] text-rose-300 break-words">{ticket.result.error}</p>
						{/if}
						<div class="mt-3 flex justify-between items-center pt-2 border-t border-surface-900 text-[10px] text-slate-500 font-mono">
							<span>{ticket.id.slice(0, 8)}</span>
							<button type="button" class="text-slate-400 hover:text-slate-200 flex items-center gap-1" onclick={() => (xrayId = ticket.id)}>
								<Eye class="w-3 h-3" /><span>Audit Trail</span>
							</button>
						</div>
					</div>
				{:else}
					<p class="text-xs text-slate-500 font-mono text-center py-6">{loading ? 'Loading…' : 'Nothing completed yet'}</p>
				{/each}
			</div>
		</div>
	</div>
</div>

{#if drawerTicket}
	<ApprovalDrawer ticket={drawerTicket} {canDecide} onClose={() => (drawerId = null)} onApprove={approve} onReject={reject} />
{/if}

{#if xrayTicket}
	<DecisionXRayModal ticket={xrayTicket} {workspaceId} canCorrect={canDecide} onClose={() => (xrayId = null)} />
{/if}
