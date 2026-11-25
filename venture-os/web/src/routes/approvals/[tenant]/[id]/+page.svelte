<script lang="ts">
	import { enhance } from '$app/forms';
	import { invalidate } from '$app/navigation';
	import ApprovalPayload from '$lib/components/ApprovalPayload.svelte';
	import Badge from '$lib/components/Badge.svelte';
	import Money from '$lib/components/Money.svelte';
	import { needsExtraCare } from '$lib/approvals';
	import { timeAgo, titleCase } from '$lib/format';

	let { data, form } = $props();

	let note = $state('');
	let busy = $state<'approve' | 'reject' | null>(null);
	let confirming = $state(false);

	const a = $derived(data.approval);
	const pending = $derived(a.status === 'PENDING_APPROVAL' && !form?.ok);
	const careful = $derived(needsExtraCare(a.kind, a.risk, data.payload.flags));
	const outcome = $derived(form && 'ok' in form && form.ok ? form : null);
	const riskTone = { HIGH: 'bad', MEDIUM: 'warn', LOW: 'neutral' } as const;
	const noExecutor = $derived(a.kind === 'PAYMENT' || a.kind === 'CAMPAIGN' || a.kind === 'AGENT_SKILL' || a.kind === 'OTHER');

	const consequence: Record<string, string> = {
		INVOICE: 'Approving posts this invoice to the ledger (receivable, revenue and tax) and schedules the receipt in your cash-flow forecast.',
		PURCHASE_ORDER: 'Approving schedules this payment in your cash-flow forecast as a committed outflow.',
		QUOTE: 'Approving releases this quote. No delivery channel is connected yet, so you will need to send it to the customer yourself.',
		PAYMENT: 'Approving records your decision only. Venture OS cannot move money; you or your bank must make the payment.',
		CAMPAIGN: 'Approving records your decision only. Sending is not automated yet.',
		AGENT_SKILL: 'Approving records your decision. The skill change is applied by an administrator.',
		OTHER: 'Approving records your decision for the requesting agent to see.'
	};

	const done: Record<string, string> = {
		INVOICE: 'Done. The invoice was posted to the ledger and the receipt is scheduled in your cash-flow forecast.',
		PURCHASE_ORDER: 'Done. The payment is scheduled in your cash-flow forecast.',
		QUOTE: 'Done. The quote is released. Send it to the customer yourself: no delivery channel is connected yet.'
	};

	function onSubmit({ formData }: { formData: FormData }) {
		busy = formData.get('decision') === 'approve' ? 'approve' : 'reject';
		return async ({ update }: { update: (o?: { reset?: boolean }) => Promise<void> }) => {
			await update({ reset: false });
			busy = null;
			confirming = false;
			await invalidate('app:shell');
		};
	}
</script>

<svelte:head><title>{a.title} · Venture OS</title></svelte:head>

<nav class="mb-4 text-sm"><a href="/approvals?b={a.tenantId}" class="text-ink-2 hover:underline">← {a.business} approvals</a></nav>

<div class="grid gap-6 lg:grid-cols-[1fr_22rem]">
	<article class="rounded-xl border border-line bg-surface p-6">
		<div class="flex flex-wrap items-center gap-2">
			<h1 class="text-xl font-semibold tracking-tight">{a.title}</h1>
			<Badge value={a.risk} tone={riskTone[a.risk as keyof typeof riskTone] ?? 'neutral'} label="{titleCase(a.risk)} risk" />
			<Badge value={outcome ? outcome.status : a.status} />
		</div>
		<p class="mt-1 text-sm text-ink-2">{a.summary}</p>
		<p class="mt-1 text-xs text-ink-3">{titleCase(a.kind)} · requested by {a.requestedBy} · {timeAgo(a.createdAt)} · {a.business}</p>
		{#if a.amount !== null}<p class="mt-4 text-3xl font-semibold"><Money minor={a.amount} currency={a.currency} /></p>{/if}

		<div class="mt-6 border-t border-line pt-5">
			<ApprovalPayload view={data.payload} currency={a.currency} />
		</div>
	</article>

	<aside class="space-y-4">
		{#if outcome}
			<section class="rounded-xl border border-line bg-surface p-5" aria-live="polite">
				<h2 class="font-semibold">{outcome.approved ? 'Approved' : 'Rejected'}</h2>
				{#if outcome.approved && outcome.executed}
					<p class="mt-2 text-sm text-good">{done[a.kind] ?? 'Done.'}</p>
				{:else if outcome.approved && outcome.executionError}
					<p class="mt-2 text-sm text-bad">Approved, but running it failed: {outcome.executionError}</p>
				{:else if outcome.approved}
					<p class="mt-2 text-sm text-ink-2">{consequence[a.kind]}</p>
				{:else}
					<p class="mt-2 text-sm text-ink-2">Your agent will see the rejection{note ? ' and your note' : ''}. Nothing was done.</p>
				{/if}
				<a href="/approvals" class="mt-4 inline-block text-sm font-medium text-accent hover:underline">Back to the inbox →</a>
			</section>
		{:else if pending}
			<section class="rounded-xl border border-line bg-surface p-5">
				<h2 class="font-semibold">Your decision</h2>
				<p class="mt-2 text-sm text-ink-2">{consequence[a.kind] ?? ''}</p>
				{#if form && 'message' in form && form.message}<p class="mt-3 rounded-lg bg-bad-bg p-3 text-sm text-bad" role="alert">{form.message}</p>{/if}

				<form method="POST" action="?/decide" class="mt-4 space-y-3" use:enhance={onSubmit}>
					<input type="hidden" name="payloadHash" value={data.payloadHash} />
					<label class="block text-sm">
						<span class="text-ink-2">Note for your agent (optional)</span>
						<textarea name="note" bind:value={note} rows="3" maxlength="2000" class="mt-1 w-full rounded-lg border border-line bg-bg p-2 text-sm"></textarea>
					</label>

					{#if careful && !confirming}
						<button type="button" class="w-full rounded-lg bg-accent px-4 py-2 text-sm font-medium text-accent-ink hover:opacity-90" onclick={() => (confirming = true)}>Review and approve…</button>
					{:else if careful}
						<div class="rounded-lg border border-warn/40 bg-warn-bg p-3 text-sm text-warn" role="alert">
							{#if a.kind === 'PAYMENT'}This is a payment{:else}This request is high risk or was flagged{/if}. Please confirm you have reviewed the details on the left.
						</div>
						<button
							class="w-full rounded-lg bg-accent px-4 py-2 text-sm font-medium text-accent-ink hover:opacity-90 disabled:opacity-60"
							name="decision" value="approve" disabled={busy !== null}
						>{busy === 'approve' ? 'Approving…' : 'Yes, approve'}</button>
					{:else}
						<button
							class="w-full rounded-lg bg-accent px-4 py-2 text-sm font-medium text-accent-ink hover:opacity-90 disabled:opacity-60"
							name="decision" value="approve" disabled={busy !== null}
						>{busy === 'approve' ? 'Approving…' : 'Approve'}</button>
					{/if}
					<button
						class="w-full rounded-lg border border-line px-4 py-2 text-sm hover:bg-surface-2 disabled:opacity-60"
						name="decision" value="reject" disabled={busy !== null}
					>{busy === 'reject' ? 'Rejecting…' : 'Reject'}</button>
				</form>
			</section>
		{:else}
			<section class="rounded-xl border border-line bg-surface p-5">
				<h2 class="font-semibold">Decision</h2>
				<p class="mt-2 text-sm text-ink-2">
					{titleCase(a.status)}{data.decidedBy ? ' by ' + (data.decidedBy === data.user?.sub ? 'you' : data.decidedBy) : ''}.
					{#if data.decisionNote}“{data.decisionNote}”{/if}
				</p>
				{#if a.status === 'APPROVED' && noExecutor}
					<p class="mt-3 rounded-lg bg-surface-2 p-3 text-sm text-ink-2">Approved. This kind of request is completed outside Venture OS (nothing was executed automatically).</p>
				{/if}
				{#if data.result}
					<details class="mt-3 text-xs text-ink-3"><summary class="cursor-pointer">Execution result</summary><pre class="mt-2 overflow-x-auto rounded bg-surface-2 p-2">{data.result}</pre></details>
				{/if}
			</section>
		{/if}
		<p class="px-1 text-xs text-ink-3">
			Every number on this page was computed by Venture OS's deterministic engine, not written by an agent. Your approval is bound to exactly this content (hash <code class="font-mono">{data.payloadHash.slice(0, 10)}…</code>).
		</p>
	</aside>
</div>
