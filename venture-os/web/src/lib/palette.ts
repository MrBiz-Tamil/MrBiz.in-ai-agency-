// Command palette ranking: a small, dependency-free fuzzy matcher.

export interface PaletteItem {
	id: string;
	label: string;
	hint?: string;
	group: string;
	/** Extra words that should match but are not displayed. */
	keywords?: string;
	href?: string;
	run?: () => void;
}

function isWordStart(t: string, i: number): boolean {
	return i === 0 || /[\s\-_/:.]/.test(t[i - 1]);
}

/**
 * Scores how well `query` matches `text` (higher is better, 0 = no match).
 * Rewards exact, prefix and word-start matches and consecutive characters. All query characters must appear in order.
 */
export function score(query: string, text: string): number {
	const q = query.trim().toLowerCase();
	if (!q) return 1;
	const t = text.toLowerCase();
	if (t === q) return 1000;
	if (t.startsWith(q)) return 500 - Math.min(t.length - q.length, 100);
	const at = t.indexOf(q);
	if (at >= 0) return (isWordStart(t, at) ? 300 : 200) - Math.min(at, 100);

	let ti = 0;
	let total = 0;
	let run = 0;
	for (const ch of q) {
		const found = t.indexOf(ch, ti);
		if (found < 0) return 0;
		run = found === ti ? run + 1 : 0;
		total += 1 + run * 2 + (isWordStart(t, found) ? 3 : 0);
		ti = found + 1;
	}
	return Math.max(1, Math.min(total, 150) - Math.floor((t.length - q.length) / 8));
}

/** Filters and ranks items for a query; with an empty query keeps the given order. */
export function rank(items: PaletteItem[], query: string, limit = 40): PaletteItem[] {
	if (!query.trim()) return items.slice(0, limit);
	return items
		.map((it) => ({ it, s: Math.max(score(query, it.label), score(query, `${it.label} ${it.keywords ?? ''} ${it.hint ?? ''}`) * 0.6) }))
		.filter((x) => x.s > 0)
		.sort((a, b) => b.s - a.s)
		.slice(0, limit)
		.map((x) => x.it);
}
