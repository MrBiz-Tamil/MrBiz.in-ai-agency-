<script lang="ts">
	import { CheckCircle2, XCircle, AlertTriangle, ShieldAlert } from 'lucide-svelte';
	import { policyOf, type Ticket } from '$lib/graphql/operations';

	interface Props {
		ticket: Ticket;
		canDecide: boolean;
		onClose: () => void;
		/** Resolve with an error message to show, or null on success. */
		onApprove: (ticketId: string, reason: string) => Promise<string | null>;
		onReject: (ticketId: string, reason: string) => Promise<string | null>;
	}

	let { ticket, canDecide, onClose, onApprove, onReject }: Props = $props();

	let reason = $state('');
	let busy = $state(false);
	let error = $state<string | null>(null);

	async function decide(kind: 'approve' | 'reject') {
		if (busy) return;
		if (kind === 'reject' && reason.trim() === '') {
			error = 'Give a reason for rejecting.';
			return;
		}
		busy = true;
		error = null;
		const err = await (kind === 'approve' ? onApprove : onReject)(ticket.id, reason.trim());
		busy = false;
		if (err) error = err;
		else onClose();
	}

	function onKeydown(e: KeyboardEvent) {
		if (e.key === 'Escape') onClose();
	}

	function onBackdrop(e: MouseEvent) {
		if (e.target === e.currentTarget) onClose();
	}

	let policy = $derived(policyOf(ticket));
	let isConflict = $derived(ticket.status === 'CONFLICT_HOLD');
	let payloadText = $derived(JSON.stringify(ticket.payload, null, 2));
	let stillOpen = $derived(ticket.status === 'PENDING_APPROVAL' || ticket.status === 'CONFLICT_HOLD');
</script>

<svelte:window onkeydown={onKeydown} />

<div class="fixed inset-0 bg-black/60 backdrop-blur-sm z-[60] flex justify-end" role="presentation" onclick={onBackdrop}>
	<div
		class="w-full max-w-lg bg-surface-900 border-l border-surface-800 h-full p-6 flex flex-col justify-between overflow-y-auto"
		role="dialog"
		aria-modal="true"
		aria-labelledby="drawer-title"
	>
		<div>
			<div class="flex justify-between items-start pb-4 border-b border-surface-800">
				<div>
					<div class="flex items-center gap-2">
						<span class="text-xs px-2 py-0.5 rounded bg-amber-950 text-amber-300 font-semibold border border-amber-800/80">
							{ticket.status}
						</span>
						<span class="text-xs text-slate-400 font-mono">Priority: P{ticket.priority}</span>
					</div>
					<h2 id="drawer-title" class="text-lg font-bold text-slate-100 mt-2">{ticket.actionType}</h2>
					<p class="text-xs text-slate-400 font-mono mt-0.5">Ticket ID: {ticket.id}</p>
				</div>
				<button type="button" class="text-slate-400 hover:text-slate-200 text-sm font-mono px-2 py-1 rounded bg-surface-800" onclick={onClose}>
					ESC
				</button>
			</div>

			{#if ticket.conflictDiff}
				<div class="mt-6 p-4 rounded-xl bg-amber-950/20 border border-amber-800/60">
					<div class="flex items-center gap-2 text-amber-400 text-xs font-semibold mb-3">
						<AlertTriangle class="w-4 h-4" />
						<span>{isConflict ? 'Cross-System Data Conflict' : 'Policy Limit Exceeded'}</span>
					</div>
					<div class="grid grid-cols-2 gap-3 text-xs">
						<div class="p-3 bg-surface-950 rounded-lg border border-surface-800">
							<span class="text-slate-400 block font-mono text-[10px] uppercase">{ticket.conflictDiff.systemA}</span>
							<span class="text-slate-200 font-medium mt-1 block">{ticket.conflictDiff.valueA || '—'}</span>
						</div>
						<div class="p-3 bg-surface-950 rounded-lg border border-surface-800">
							<span class="text-slate-400 block font-mono text-[10px] uppercase">{ticket.conflictDiff.systemB}</span>
							<span class="text-amber-300 font-medium mt-1 block">{ticket.conflictDiff.valueB}</span>
						</div>
					</div>
				</div>
			{/if}

			<div class="mt-6 p-4 rounded-xl bg-surface-950 border border-surface-800">
				<div class="flex items-center gap-2 text-indigo-400 text-xs font-semibold mb-2">
					<ShieldAlert class="w-4 h-4" />
					<span>Governance Trigger</span>
				</div>
				<p class="text-xs text-slate-300 leading-relaxed">
					{#if policy}
						Deterministic rule <code class="text-indigo-300 bg-surface-900 px-1 py-0.5 rounded">{policy.rule}</code> paused this action: {policy.reason}.
						{#if policy.assignedRole}<span class="block mt-1 text-slate-500">Assigned to: {policy.assignedRole}</span>{/if}
					{:else}
						This action was held for human review. No specific policy rule was recorded.
					{/if}
				</p>
			</div>

			<div class="mt-6">
				<span class="text-xs font-mono text-slate-400 block mb-2">Action Payload:</span>
				<pre class="p-3 bg-surface-950 rounded-lg border border-surface-800 text-[11px] font-mono text-slate-300 overflow-x-auto max-h-48">{payloadText}</pre>
			</div>
		</div>

		<div class="pt-6 border-t border-surface-800 space-y-3">
			{#if !stillOpen}
				<p class="text-xs text-slate-400" role="status">This ticket is no longer awaiting a decision.</p>
			{:else if !canDecide}
				<p class="text-xs text-slate-400" role="status">Your role can view this ticket but cannot approve or reject it.</p>
			{:else}
				<label for="decision-reason" class="block text-[10px] uppercase font-mono text-slate-400">Reason (required to reject)</label>
				<input id="decision-reason" type="text" maxlength="500" bind:value={reason}
					class="w-full bg-surface-950 border border-surface-800 rounded-lg px-3 py-2 text-xs text-slate-200 focus:outline-none focus:border-indigo-500" />
				{#if error}
					<p class="text-xs text-rose-400" role="alert">{error}</p>
				{/if}
				<button type="button" disabled={busy}
					class="w-full flex items-center justify-center gap-2 py-2.5 px-4 rounded-lg bg-emerald-600 hover:bg-emerald-500 font-semibold text-xs text-white transition-colors shadow-lg shadow-emerald-600/20 disabled:opacity-50"
					onclick={() => decide('approve')}>
					<CheckCircle2 class="w-4 h-4" />
					<span>{busy ? 'Working…' : 'Approve & Execute'}</span>
				</button>
				<button type="button" disabled={busy}
					class="w-full flex items-center justify-center gap-2 py-2.5 px-4 rounded-lg bg-surface-800 hover:bg-rose-950/60 hover:text-rose-400 hover:border-rose-800/80 border border-surface-700 font-semibold text-xs text-slate-300 transition-colors disabled:opacity-50"
					onclick={() => decide('reject')}>
					<XCircle class="w-4 h-4" />
					<span>Reject & Cancel Execution</span>
				</button>
			{/if}
		</div>
	</div>
</div>
