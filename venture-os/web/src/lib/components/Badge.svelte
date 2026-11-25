<script lang="ts">
	import { STATUS_TONE } from '$lib/types';
	import { titleCase } from '$lib/format';

	let { value, tone, label }: { value: string; tone?: 'good' | 'warn' | 'bad' | 'neutral'; label?: string } = $props();
	const t = $derived(tone ?? STATUS_TONE[value] ?? 'neutral');
	const classes = {
		good: 'bg-good-bg text-good',
		warn: 'bg-warn-bg text-warn',
		bad: 'bg-bad-bg text-bad',
		neutral: 'bg-surface-2 text-ink-2'
	};
</script>

<span class="inline-flex items-center gap-1 rounded-full px-2 py-0.5 text-xs font-medium whitespace-nowrap {classes[t]}">
	<span class="size-1.5 rounded-full bg-current" aria-hidden="true"></span>
	{label ?? titleCase(value)}
</span>
