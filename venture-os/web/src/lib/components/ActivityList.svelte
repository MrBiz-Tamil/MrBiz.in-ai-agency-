<script lang="ts">
	import { enhance } from '$app/forms';
	import { actorLabel, isOverdue, type Activity } from '$lib/crm';
	import { formatDay, timeAgo, titleCase } from '$lib/format';

	let {
		activities,
		canWrite,
		me,
		empty = 'Nothing recorded yet.',
		parentLink
	}: {
		activities: Activity[];
		canWrite: boolean;
		me: string;
		empty?: string;
		/** Where an entry lives, for lists that mix deals and companies (the Tasks tab). */
		parentLink?: (a: Activity) => { href: string; label: string } | null;
	} = $props();
</script>

{#if activities.length === 0}
	<p class="text-sm text-ink-2">{empty}</p>
{:else}
	<ul class="divide-y divide-line">
		{#each activities as a (a.id)}
			<li class="py-3">
				<div class="flex items-start justify-between gap-3">
					<div class="min-w-0">
						<p class="text-sm">
							<span class="mr-1.5 rounded bg-surface-2 px-1.5 py-0.5 text-[11px] font-medium text-ink-2">{titleCase(a.kind)}</span>
							<span class="font-medium {a.completed_at ? 'text-ink-3 line-through' : ''}">{a.subject}</span>
						</p>
						{#if a.body}
							<p class="mt-1 text-sm whitespace-pre-wrap text-ink-2">{a.body}{#if a.body_truncated}<span class="text-ink-3"> … (shortened)</span>{/if}</p>
						{/if}
						<p class="mt-1 text-xs text-ink-3">
							{actorLabel(a.created_by, me)} · {timeAgo(a.created_at)}
							{#if a.due_at && !a.completed_at}
								· <span class={isOverdue(a) ? 'font-medium text-bad' : ''}>{isOverdue(a) ? 'Overdue, was due' : 'Due'} {formatDay(a.due_at)}</span>
							{/if}
							{#if a.completed_at}· Done {timeAgo(a.completed_at)}{/if}
							{#if parentLink}{@const l = parentLink(a)}{#if l}· <a href={l.href} class="text-accent hover:underline">{l.label}</a>{/if}{/if}
						</p>
					</div>
					{#if canWrite && a.kind === 'TASK' && !a.completed_at}
						<form method="POST" action="?/complete" use:enhance>
							<input type="hidden" name="activity_id" value={a.id} />
							<button class="shrink-0 rounded-lg border border-line px-2.5 py-1 text-xs hover:bg-surface-2" aria-label="Mark “{a.subject}” done">Done</button>
						</form>
					{/if}
				</div>
			</li>
		{/each}
	</ul>
{/if}
