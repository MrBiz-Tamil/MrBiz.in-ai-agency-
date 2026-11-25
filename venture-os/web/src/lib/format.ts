// Money and time formatting. Amounts arrive as int64 (bigint) minor units; they are converted to an exact
// decimal string BEFORE formatting, so nothing ever passes through a floating-point number.

const digitsCache = new Map<string, number>();

/** Number of minor-unit digits for a currency (2 for USD/INR, 0 for JPY, 3 for KWD). */
export function minorDigits(currency: string): number {
	let d = digitsCache.get(currency);
	if (d === undefined) {
		try {
			d = new Intl.NumberFormat('en', { style: 'currency', currency }).resolvedOptions().maximumFractionDigits ?? 2;
		} catch {
			d = 2;
		}
		digitsCache.set(currency, d);
	}
	return d;
}

/** "1180562" with 2 digits -> "11805.62" (exact). */
export function minorToDecimal(amount: bigint, digits: number): string {
	const neg = amount < 0n;
	const abs = (neg ? -amount : amount).toString().padStart(digits + 1, '0');
	const whole = abs.slice(0, abs.length - digits);
	const frac = digits ? '.' + abs.slice(abs.length - digits) : '';
	return (neg ? '-' : '') + whole + frac;
}

export function formatMoney(amount: bigint | number, currency: string, locale = 'en-IN'): string {
	const big = typeof amount === 'bigint' ? amount : BigInt(Math.trunc(amount));
	if (!currency) return big.toString();
	const decimal = minorToDecimal(big, minorDigits(currency));
	try {
		// NumberFormat accepts decimal strings and keeps full precision (ES2023).
		return new Intl.NumberFormat(locale, { style: 'currency', currency }).format(decimal as unknown as number);
	} catch {
		return `${currency} ${decimal}`;
	}
}

/** Compact form for dashboards (lakh/crore for en-IN, K/M elsewhere). */
export function formatMoneyCompact(amount: bigint, currency: string, locale = 'en-IN'): string {
	const decimal = minorToDecimal(amount, minorDigits(currency));
	try {
		return new Intl.NumberFormat(locale, { style: 'currency', currency, notation: 'compact', maximumFractionDigits: 1 }).format(
			decimal as unknown as number
		);
	} catch {
		return formatMoney(amount, currency, locale);
	}
}

export function formatBps(bps: number): string {
	const sign = bps < 0 ? '-' : '';
	const a = Math.abs(bps);
	return `${sign}${Math.floor(a / 100)}.${String(a % 100).padStart(2, '0')}%`;
}

export function timeAgo(iso: string, now = Date.now()): string {
	const t = Date.parse(iso);
	if (Number.isNaN(t)) return '';
	const s = Math.max(0, Math.round((now - t) / 1000));
	if (s < 60) return 'just now';
	const m = Math.round(s / 60);
	if (m < 60) return `${m}m ago`;
	const h = Math.round(m / 60);
	if (h < 24) return `${h}h ago`;
	const d = Math.round(h / 24);
	return d < 30 ? `${d}d ago` : new Date(t).toLocaleDateString('en-GB', { day: 'numeric', month: 'short', year: 'numeric' });
}

export const titleCase = (s: string) => s.toLowerCase().replace(/_/g, ' ').replace(/^\w/, (c) => c.toUpperCase());

/** A calendar day such as "10 Mar 2026". Date-only values (2026-03-10) are read as UTC so they never shift a day. */
export function formatDay(iso: string): string {
	const t = Date.parse(iso);
	if (Number.isNaN(t)) return '';
	return new Date(t).toLocaleDateString('en-GB', { day: 'numeric', month: 'short', year: 'numeric', timeZone: 'UTC' });
}
