<script lang="ts">
	import { ShieldCheck, X, Bot } from 'lucide-svelte';
	import { getContextClient } from '@urql/svelte';
	import { onMount, onDestroy } from 'svelte';
	import {
		Q_XRAY, S_AUDIT, Q_ROSTER, M_CORRECTION, errorMessage,
		type Ticket, type AuditLog, type Agent
	} from '$lib/graphql/operations';

	interface Props {
		ticket: Ticket;
		workspaceId: string;
		canCorrect: boolean;
		onClose: () => void;
	}
	let { ticket, workspaceId, canCorrect, onClose }: Props = $props();

	const client = getContextClient();

	let steps = $state<AuditLog[]>([]);
	let loading = $state(true);
	let loadError = $state<string | null>(null);
	let agents = $state<Agent[]>([]);
	let agentId = $state('');
	let note = $state('');
	let saving = $state(false);
	let saved = $state(false);
	let saveError = $state<string | null>(null);
	let closeTimer: ReturnType<typeof setTimeout> | undefined;
	let unsubscribe: (() => void) | undefined;

	function merge(incoming: AuditLog) {
		if (!steps.some((s) => s.id === incoming.id)) steps = [...steps, incoming].sort((a, b) => a.stepNumber - b.stepNumber);
	}

	onMount(async () => {
		const [xray, roster] = await Promise.all([
			client.query(Q_XRAY, { id: ticket.id }, { requestPolicy: 'network-only' }).toPromise(),
			client.query(Q_ROSTER, { ws: workspaceId }, { requestPolicy: 'network-only' }).toPromise()
		]);
		loading = false;
		if (xray.error) loadError = errorMessage(xray.error);
		else steps = xray.data?.decisionXRay ?? [];
		agents = roster.data?.agentRoster ?? [];
		agentId = ticket.assignedAgent?.id ?? agents[0]?.id ?? '';

		// Live: new steps appear while the modal is open.
		const sub = client.subscription(S_AUDIT, { id: ticket.id }).subscribe((r) => {
			if (r.data?.auditLogAppended) merge(r.data.auditLogAppended);
		});
		unsubscribe = () => sub.unsubscribe();
	});

	onDestroy(() => {
		clearTimeout(closeTimer);
		unsubscribe?.();
	});

	let canSubmit = $derived(canCorrect && !!agentId && note.trim().length > 0 && !saving && !saved);

	async function submitCorrection() {
		if (!canSubmit) return;
		saving = true;
		saveError = null;
		const res = await client
			.mutation(M_CORRECTION, { input: { ticketId: ticket.id, agentId, correctionNote: note.trim(), updatePlaybookInNotion: false } })
			.toPromise();
		saving = false;
		if (res.error) {
			saveError = errorMessage(res.error);
			return;
		}
		saved = true;
		closeTimer = setTimeout(onClose, 1200);
	}

	/** A readable one-liner for an audit step. Plain text only: nothing here is rendered as HTML. */
	function describe(s: AuditLog): string {
		const o = s.outputSanitized ?? {};
		if (s.toolName === 'kernel.policy_evaluator') return o.reason ? String(o.reason) : 'Deterministic policy evaluated.';
		if (s.toolName === 'approval.gate') {
			const who = o.by ? `${o.by}${o.role ? ` (${o.role})` : ''}` : 'a reviewer';
			const why = o.reason ? `: "${o.reason}"` : '';
			return s.policyDecision === 'REJECTED' ? `Rejected by ${who}${why}` : `Approved by ${who}${why}`;
		}
		if (s.toolName === 'human.correction') return `Correction from ${o.by ?? 'a reviewer'}: ${o.note ?? ''}`;
		if (s.policyDecision === 'EXECUTION_FAILED') return `Execution failed: ${o.error ?? 'unknown error'}`;
		const text = Object.keys(o).length ? JSON.stringify(o) : '';
		return text.length > 240 ? text.slice(0, 240) + '…' : text || 'Executed.';
	}

	function decisionClass(d: string): string {
		if (d.startsWith('ALLOW') || d === 'APPROVED' || d === 'MODIFIED_AND_APPROVED') return 'bg-emerald-950 text-emerald-400';
		if (d === 'REJECTED' || d === 'REJECT' || d === 'EXECUTION_FAILED') return 'bg-rose-950 text-rose-300';
		return 'bg-amber-950 text-amber-300';
	}

	function onKeydown(e: KeyboardEvent) {
		if (e.key === 'Escape') onClose();
	}
	function onBackdrop(e: MouseEvent) {
		if (e.target === e.currentTarget) onClose();
	}
</script>

<svelte:window onkeydown={onKeydown} />

<div class="fixed inset-0 bg-black/70 backdrop-blur-sm z-[60] flex items-center justify-center p-6" role="presentation" onclick={onBackdrop}>
	<div class="w-full max-w-2xl bg-surface-900 border border-surface-800 rounded-2xl p-6 flex flex-col max-h-[85vh] overflow-hidden shadow-2xl"
		role="dialog" aria-modal="true" aria-labelledby="xray-title">
		<div class="flex justify-between items-center pb-4 border-b border-surface-800">
			<div>
				<h2 id="xray-title" class="text-lg font-bold text-slate-100 flex items-center gap-2">
					<ShieldCheck class="w-5 h-5 text-indigo-400" />
					<span>Decision X-Ray: Audit Inspection</span>
				</h2>
				<p class="text-xs text-slate-400 font-mono mt-0.5">{ticket.actionType} · {ticket.id}</p>
			</div>
			<button type="button" class="text-slate-400 hover:text-slate-200 p-1.5 rounded-lg bg-surface-800" aria-label="Close" onclick={onClose}>
				<X class="w-4 h-4" />
			</button>
		</div>

		<div class="space-y-4 my-6 overflow-y-auto flex-1 pr-2">
			{#if loading}
				<p class="text-xs text-slate-500 font-mono text-center py-6">Loading audit trail…</p>
			{:else if loadError}
				<p class="text-xs text-rose-400 text-center py-6" role="alert">{loadError}</p>
			{:else if steps.length === 0}
				<p class="text-xs text-slate-500 font-mono text-center py-6">No audit steps recorded for this ticket.</p>
			{/if}
			{#each steps as step (step.id)}
				<div class="p-4 rounded-xl bg-surface-950 border border-surface-800">
					<div class="flex justify-between items-center mb-2">
						<div class="flex items-center gap-2">
							<span class="w-5 h-5 rounded-full bg-surface-800 text-[10px] font-mono flex items-center justify-center text-slate-300 font-bold">{step.stepNumber}</span>
							<span class="text-xs font-semibold text-slate-200 font-mono">{step.toolName ?? 'step'}</span>
						</div>
						<span class="text-[10px] font-mono px-2 py-0.5 rounded {decisionClass(step.policyDecision)}">{step.policyDecision}</span>
					</div>
					<p class="text-xs text-slate-300 leading-relaxed pl-7 break-words">{describe(step)}</p>
					<div class="mt-3 pl-7 flex items-center gap-4 text-[10px] font-mono text-slate-500">
						<span>Latency: {step.latencyMs}ms</span>
						<span>Spend: ${step.tokenCost}</span>
						<span>{new Date(step.createdAt).toLocaleTimeString()}</span>
					</div>
				</div>
			{/each}
		</div>

		<div class="pt-4 border-t border-surface-800">
			<label for="feedback" class="mb-1 flex items-center gap-1.5 text-xs font-medium text-slate-300">
				<Bot class="w-4 h-4 text-indigo-400" />
				<span>Record a correction on this ticket's audit trail (Notion SOP sync is not connected):</span>
			</label>
			{#if !canCorrect}
				<p class="text-xs text-slate-500">Your role can view audit trails but cannot add corrections.</p>
			{:else}
				<div class="flex gap-2">
					<select aria-label="Agent being corrected" bind:value={agentId}
						class="bg-surface-950 border border-surface-800 rounded-lg px-2 py-2 text-xs text-slate-200 max-w-[11rem]">
						{#each agents as a (a.id)}<option value={a.id}>{a.roleName}</option>{/each}
					</select>
					<input id="feedback" type="text" maxlength="1000" placeholder="e.g. Always confirm the order number before refunding"
						class="flex-1 bg-surface-950 border border-surface-800 rounded-lg px-3 py-2 text-xs text-slate-200 focus:outline-none focus:border-indigo-500"
						bind:value={note} onkeydown={(e) => e.key === 'Enter' && submitCorrection()} />
					<button type="button" disabled={!canSubmit} onclick={submitCorrection}
						class="px-4 py-2 bg-indigo-600 hover:bg-indigo-500 text-white font-semibold text-xs rounded-lg transition-colors disabled:opacity-50">
						{saved ? 'Recorded' : saving ? 'Saving…' : 'Record'}
					</button>
				</div>
				{#if saveError}<p class="text-xs text-rose-400 mt-2" role="alert">{saveError}</p>{/if}
			{/if}
		</div>
	</div>
</div>
