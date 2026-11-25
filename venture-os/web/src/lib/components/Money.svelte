<script lang="ts">
	import { formatMoney, formatMoneyCompact } from '$lib/format';

	let { minor, currency, compact = false, class: cls = '' }: { minor: bigint; currency: string; compact?: boolean; class?: string } = $props();
	const text = $derived(compact ? formatMoneyCompact(minor, currency) : formatMoney(minor, currency));
</script>

<span class="tabular {cls}" title={compact ? formatMoney(minor, currency) : undefined}>{text}</span>
