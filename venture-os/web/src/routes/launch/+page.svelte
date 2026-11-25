<script lang="ts">
	import { replaceState } from '$app/navigation';
	import { tick } from 'svelte';
	import { parseBlueprint, type Blueprint } from '$lib/blueprint';
	import BlueprintPreview from '$lib/components/BlueprintPreview.svelte';
	import ProvisionProgress from '$lib/components/ProvisionProgress.svelte';
	import type { StepView } from '$lib/types';

	let { data } = $props();

	interface Turn {
		role: 'founder' | 'assistant';
		content: string;
	}
	type Stage = 'chat' | 'review' | 'launching' | 'done' | 'failed';

	const GREETING: Turn = {
		role: 'assistant',
		content: 'Hi, I’m the Venture Architect. Tell me about the business you want to start: what you will sell, to whom, and where. I’ll design the operating model and show it taking shape on the right.'
	};
	const EXAMPLES = [
		'I want to export whole spices from Kerala to wholesalers in Europe.',
		'I’m starting a B2B SaaS that automates invoice reconciliation for small accountancies.',
		'I plan to sell handmade skincare online across India, shipping from Pune.'
	];

	const resume = $derived(data.resume);
	let tenantId = $state<string | null>(null);
	let sessionId = $state<string | null>(null);
	let turns = $state<Turn[]>([]);
	let draft = $state<Blueprint | null>(null);
	let ready = $state(false);
	let problems = $state<string[]>([]);
	let input = $state('');
	let sending = $state(false);
	let error = $state('');
	let stage = $state<Stage>('chat');
	let blueprintId = $state('');
	let hash = $state('');
	let steps = $state<StepView[]>([]);
	let failure = $state('');
	let scroller: HTMLElement | undefined = $state();

	const startKey = crypto.randomUUID();
	let pendingTurn = $state<{ message: string; key: string } | null>(null);

	// Initialise from a resumed session (re-runs only when navigating to a different business).
	$effect(() => {
		if (resume) {
			tenantId = resume.tenantId;
			sessionId = resume.sessionId;
			turns = resume.turns.map((t) => ({ role: t.role === 'founder' ? 'founder' : 'assistant', content: t.content }));
			draft = parseBlueprint(resume.draft);
			ready = draft !== null && turns.length > 0;
		}
	});

	async function post<T>(action: string, body: object): Promise<T> {
		const res = await fetch(`/launch/api/${action}`, { method: 'POST', headers: { 'content-type': 'application/json' }, body: JSON.stringify(body) });
		const out = await res.json().catch(() => ({ message: 'Unexpected response from the server.' }));
		if (!res.ok) throw Object.assign(new Error(out.message ?? 'Request failed'), { status: res.status });
		return out as T;
	}

	async function scrollDown() {
		await tick();
		scroller?.scrollTo({ top: scroller.scrollHeight, behavior: 'smooth' });
	}

	async function send(text = input) {
		const message = text.trim();
		if (!message || sending) return;
		sending = true;
		error = '';
		try {
			if (!tenantId || !sessionId) {
				const s = await post<{ tenantId: string; sessionId: string }>('start', { key: startKey });
				tenantId = s.tenantId;
				sessionId = s.sessionId;
				replaceState(`/launch?b=${s.tenantId}`, {});
			}
			// A retry of the same message reuses its turn key, so a lost response never double-counts the turn.
			if (!pendingTurn || pendingTurn.message !== message) pendingTurn = { message, key: crypto.randomUUID() };
			const r = await post<{ assistant: string; draft: string; changed: boolean; ready: boolean; problems: string[] }>('converse', {
				tenantId, sessionId, message, turnKey: pendingTurn.key
			});
			pendingTurn = null;
			turns.push({ role: 'founder', content: message }, { role: 'assistant', content: r.assistant });
			input = '';
			const parsed = parseBlueprint(r.draft);
			if (parsed) draft = parsed;
			ready = r.ready;
			problems = r.problems ?? [];
			scrollDown();
		} catch (e) {
			error = (e as Error).message;
		} finally {
			sending = false;
		}
	}

	function onInputKey(e: KeyboardEvent) {
		if (e.key === 'Enter' && !e.shiftKey && !e.isComposing) {
			e.preventDefault();
			send();
		}
	}

	async function review() {
		if (!tenantId || !sessionId) return;
		error = '';
		sending = true;
		try {
			const r = await post<{ blueprintId: string; hash: string }>('submit', { tenantId, sessionId, key: `submit-${sessionId}-${turns.length}` });
			blueprintId = r.blueprintId;
			hash = r.hash;
			stage = 'review';
		} catch (e) {
			error = (e as Error).message;
		} finally {
			sending = false;
		}
	}

	async function launch() {
		if (!tenantId) return;
		error = '';
		failure = '';
		stage = 'launching';
		steps = [];
		let poll: ReturnType<typeof setInterval> | undefined;
		try {
			await post('approve', { tenantId, blueprintId, hash });
			poll = setInterval(async () => {
				try {
					const res = await fetch(`/launch/api/status?tenantId=${tenantId}`);
					if (res.ok) steps = (await res.json()).steps;
				} catch {
					/* polling is best effort */
				}
			}, 700);
			const r = await post<{ status: string; steps: StepView[] }>('provision', { tenantId, blueprintId, key: `launch-${blueprintId}-${crypto.randomUUID()}` });
			steps = r.steps;
			stage = 'done';
		} catch (e) {
			failure = (e as Error).message;
			try {
				const res = await fetch(`/launch/api/status?tenantId=${tenantId}`);
				if (res.ok) steps = (await res.json()).steps;
			} catch {
				/* keep what we have */
			}
			stage = 'failed';
		} finally {
			clearInterval(poll);
		}
	}
</script>

<svelte:head><title>Launch a business · Venture OS</title></svelte:head>

<div class="mb-4">
	<h1 class="text-2xl font-semibold tracking-tight">Launch a new business</h1>
	<p class="mt-1 text-sm text-ink-2">Explain it in your own words. Nothing is created until you approve the plan.</p>
</div>

<div class="grid gap-6 lg:grid-cols-[minmax(0,1fr)_minmax(0,26rem)]">
	<!-- Conversation -->
	<section class="flex h-[calc(100vh-14rem)] min-h-[28rem] flex-col rounded-xl border border-line bg-surface" aria-label="Conversation">
		<div bind:this={scroller} class="flex-1 space-y-4 overflow-y-auto p-4" aria-live="polite">
			{#each [GREETING, ...turns] as t, i (i)}
				<div class="flex {t.role === 'founder' ? 'justify-end' : 'justify-start'}">
					<div class="max-w-[85%] rounded-2xl px-4 py-2.5 text-sm leading-relaxed whitespace-pre-wrap {t.role === 'founder' ? 'bg-accent text-accent-ink' : 'bg-surface-2 text-ink'}">
						{t.content}
					</div>
				</div>
			{/each}
			{#if sending}
				<div class="flex justify-start"><div class="rounded-2xl bg-surface-2 px-4 py-2.5 text-sm text-ink-3" role="status"><span class="animate-pulse">The Venture Architect is thinking…</span></div></div>
			{/if}
			{#if turns.length === 0 && !sending}
				<div class="space-y-2 pt-2">
					<p class="text-xs font-medium text-ink-3">Not sure where to start? Try one:</p>
					{#each EXAMPLES as ex (ex)}
						<button type="button" class="block w-full rounded-lg border border-line px-3 py-2 text-left text-sm hover:bg-surface-2" onclick={() => send(ex)}>{ex}</button>
					{/each}
				</div>
			{/if}
			{#if problems.length}
				<div class="rounded-lg bg-warn-bg p-3 text-sm text-warn" role="status">
					<p class="font-medium">I could not make the plan fully consistent yet:</p>
					<ul class="mt-1 list-disc pl-5">{#each problems as p (p)}<li>{p}</li>{/each}</ul>
				</div>
			{/if}
		</div>

		{#if error}
			<div class="mx-4 mb-2 flex items-center justify-between gap-3 rounded-lg bg-bad-bg px-3 py-2 text-sm text-bad" role="alert">
				<span>{error}</span>
				{#if stage === 'chat' && pendingTurn}<button type="button" class="font-medium underline" onclick={() => send(pendingTurn!.message)}>Retry</button>{/if}
			</div>
		{/if}

		<form class="flex items-end gap-2 border-t border-line p-3" onsubmit={(e) => { e.preventDefault(); send(); }}>
			<label class="sr-only" for="msg">Your message</label>
			<textarea
				id="msg"
				bind:value={input}
				onkeydown={onInputKey}
				rows="2"
				maxlength="8000"
				disabled={sending || stage !== 'chat'}
				placeholder={stage === 'chat' ? 'Describe your business… (Enter to send, Shift+Enter for a new line)' : 'The plan is locked while you review it'}
				class="min-h-12 flex-1 resize-none rounded-lg border border-line bg-bg px-3 py-2 text-sm disabled:opacity-60"
			></textarea>
			<button class="rounded-lg bg-accent px-4 py-2 text-sm font-medium text-accent-ink hover:opacity-90 disabled:opacity-50" disabled={sending || !input.trim() || stage !== 'chat'}>Send</button>
		</form>
	</section>

	<!-- Live blueprint / review / progress -->
	<aside class="lg:sticky lg:top-20 lg:self-start" aria-label="Plan">
		<div class="max-h-[calc(100vh-8rem)] overflow-y-auto rounded-xl border border-line bg-surface p-5">
			{#if stage === 'chat'}
				<div class="mb-3 flex items-center justify-between">
					<h2 class="text-sm font-semibold text-ink-2">Live plan</h2>
					<span class="rounded-full px-2 py-0.5 text-xs {ready ? 'bg-good-bg text-good' : 'bg-surface-2 text-ink-3'}">{ready ? 'Ready for your review' : draft ? 'Draft: updates as we talk' : 'Waiting for your idea'}</span>
				</div>
				{#if draft}
					<BlueprintPreview blueprint={draft} />
					{#if ready}
						<button class="sticky bottom-0 mt-5 w-full rounded-lg bg-accent px-4 py-2.5 text-sm font-medium text-accent-ink shadow hover:opacity-90 disabled:opacity-50" disabled={sending} onclick={review}>Review and approve this plan</button>
					{:else}
						<p class="mt-4 text-xs text-ink-3">Keep talking to refine it. When the plan looks right, the review button appears here.</p>
					{/if}
				{:else}
					<div class="py-10 text-center text-sm text-ink-3">
						<p>Your operating plan will appear here: pricing guardrails, CRM pipelines, your AI team and workflows.</p>
					</div>
				{/if}
			{:else if stage === 'review'}
				<h2 class="text-lg font-semibold">Approve and launch</h2>
				<p class="mt-2 text-sm text-ink-2">
					You are approving this exact plan (fingerprint <code class="font-mono text-xs">{hash.slice(0, 12)}…</code>). Launching creates, for this business only:
				</p>
				<ul class="mt-3 list-disc space-y-1 pl-5 text-sm text-ink-2">
					<li>an organization and access in identity</li>
					<li>the company, chart of accounts and tax setup in the ERP</li>
					<li>a CRM workspace with your pipelines</li>
					<li>support inboxes and a seeded knowledge base</li>
					<li>an AI team of {draft?.agents.length ?? 0} agents with generated skills</li>
				</ul>
				<p class="mt-3 rounded-lg bg-surface-2 p-3 text-xs text-ink-2">Agents can only act through guarded tools. Quotes, invoices, purchase orders and payments always wait for your approval.</p>
				<div class="mt-5 flex gap-2">
					<button class="flex-1 rounded-lg bg-accent px-4 py-2.5 text-sm font-medium text-accent-ink hover:opacity-90" onclick={launch}>Approve and launch</button>
					<button class="rounded-lg border border-line px-4 py-2.5 text-sm hover:bg-surface-2" onclick={() => (stage = 'chat')}>Keep editing</button>
				</div>
			{:else if stage === 'launching'}
				<h2 class="text-lg font-semibold">Launching…</h2>
				<p class="mt-1 mb-4 text-sm text-ink-2">This usually takes under a minute. You can leave this page; it continues in the background.</p>
				<ProvisionProgress {steps} running />
			{:else if stage === 'done'}
				<h2 class="text-lg font-semibold text-good">Your business is live</h2>
				<p class="mt-1 mb-4 text-sm text-ink-2">Your AI team can now work, and will ask for your approval before anything outward-facing.</p>
				<ProvisionProgress {steps} />
				<div class="mt-5 flex gap-2">
					<a href="/b/{tenantId}" class="flex-1 rounded-lg bg-accent px-4 py-2.5 text-center text-sm font-medium text-accent-ink hover:opacity-90">Open the business</a>
					<a href="/" class="rounded-lg border border-line px-4 py-2.5 text-sm hover:bg-surface-2">Portfolio</a>
				</div>
			{:else}
				<h2 class="text-lg font-semibold text-bad">The launch did not complete</h2>
				<p class="mt-1 mb-4 text-sm text-ink-2">{failure || 'Something failed.'} Anything that was created has been rolled back, so you can safely try again.</p>
				<ProvisionProgress {steps} />
				<div class="mt-5 flex gap-2">
					<button class="flex-1 rounded-lg bg-accent px-4 py-2.5 text-sm font-medium text-accent-ink hover:opacity-90" onclick={launch}>Try again</button>
					<button class="rounded-lg border border-line px-4 py-2.5 text-sm hover:bg-surface-2" onclick={() => (stage = 'chat')}>Back to the plan</button>
				</div>
			{/if}
		</div>
	</aside>
</div>
