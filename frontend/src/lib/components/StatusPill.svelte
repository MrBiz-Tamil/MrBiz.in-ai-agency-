<script lang="ts">
	interface Props {
		status: 'HEALTHY' | 'DEGRADED' | 'CRITICAL' | 'UNKNOWN' | string;
		label?: string;
	}

	let { status = 'UNKNOWN', label = 'SYSTEM' }: Props = $props();

	let pillClass = $derived.by(() => {
		switch (status) {
			case 'HEALTHY':
				return 'bg-emerald-950/60 text-emerald-400 border-emerald-800/80';
			case 'DEGRADED':
				return 'bg-amber-950/60 text-amber-400 border-amber-800/80';
			case 'CRITICAL':
				return 'bg-rose-950/60 text-rose-400 border-rose-800/80';
			default:
				return 'bg-slate-800 text-slate-300 border-slate-700';
		}
	});

	let dotClass = $derived.by(() => {
		switch (status) {
			case 'HEALTHY':
				return 'bg-emerald-400 animate-pulse';
			case 'DEGRADED':
				return 'bg-amber-400';
			case 'CRITICAL':
				return 'bg-rose-400';
			default:
				return 'bg-slate-400';
		}
	});
</script>

<div
	class="inline-flex items-center gap-2 px-2.5 py-1 rounded-full text-xs font-semibold border {pillClass}"
	role="status"
>
	<span class="w-1.5 h-1.5 rounded-full {dotClass}"></span>
	<span>{label} {status}</span>
</div>
