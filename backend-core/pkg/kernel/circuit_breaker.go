package kernel

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/shopspring/decimal"
)

// Spend is tracked as integer micro-USD (1e-6 USD) so no floating point is involved.
var microScale = decimal.NewFromInt(1_000_000)

// budgetScript atomically checks and increments spend.
// KEYS[1] = spend key; ARGV[1] = cost (micro-USD), ARGV[2] = limit (micro-USD), ARGV[3] = ttl seconds.
// Returns {allowed(0/1), spend_after_micro}.
var budgetScript = redis.NewScript(`
local current = tonumber(redis.call('GET', KEYS[1]) or '0')
local cost = tonumber(ARGV[1])
local limit = tonumber(ARGV[2])
if current + cost > limit then
  return {0, current}
end
local after = redis.call('INCRBY', KEYS[1], cost)
if after == cost then
  redis.call('EXPIRE', KEYS[1], tonumber(ARGV[3]))
end
return {1, after}
`)

type BudgetCircuitBreaker struct {
	redisClient *redis.Client

	mu    sync.Mutex
	local map[string]int64 // used only when Redis is not configured
}

type BudgetEvaluation struct {
	Allowed         bool            `json:"allowed"`
	SpentTodayUSD   decimal.Decimal `json:"spent_today_usd"`
	DailyLimitUSD   decimal.Decimal `json:"daily_limit_usd"`
	IsWarning80     bool            `json:"is_warning_80"`
	IsCircuitBroken bool            `json:"is_circuit_broken"`
}

func NewBudgetCircuitBreaker(rdb *redis.Client) *BudgetCircuitBreaker {
	return &BudgetCircuitBreaker{redisClient: rdb, local: make(map[string]int64)}
}

func toMicro(d decimal.Decimal) int64 {
	return d.Mul(microScale).Round(0).IntPart()
}

func fromMicro(m int64) decimal.Decimal {
	return decimal.NewFromInt(m).Div(microScale)
}

// EvaluateAndIncrementBudget atomically checks the spend against the limit and, only if the
// request fits, records it. Requests that would exceed the limit are denied without being recorded.
func (b *BudgetCircuitBreaker) EvaluateAndIncrementBudget(
	ctx context.Context,
	workspaceID string,
	agentID string,
	estimatedCostUSD decimal.Decimal,
	dailyLimitUSD decimal.Decimal,
) (BudgetEvaluation, error) {
	if estimatedCostUSD.IsNegative() {
		return BudgetEvaluation{}, errors.New("estimated cost cannot be negative")
	}
	if dailyLimitUSD.IsNegative() {
		return BudgetEvaluation{}, errors.New("daily limit cannot be negative")
	}

	today := time.Now().UTC().Format("2006-01-02")
	spendKey := fmt.Sprintf("budget:%s:%s:%s", workspaceID, agentID, today)
	cost := toMicro(estimatedCostUSD)
	limit := toMicro(dailyLimitUSD)

	var allowed bool
	var spent int64

	if b.redisClient == nil {
		b.mu.Lock()
		current := b.local[spendKey]
		if current+cost > limit {
			allowed, spent = false, current
		} else {
			b.local[spendKey] = current + cost
			allowed, spent = true, current+cost
		}
		b.mu.Unlock()
	} else {
		res, err := budgetScript.Run(ctx, b.redisClient, []string{spendKey}, cost, limit, int64((48 * time.Hour).Seconds())).Int64Slice()
		if err != nil {
			return BudgetEvaluation{}, err
		}
		if len(res) != 2 {
			return BudgetEvaluation{}, errors.New("unexpected budget script result")
		}
		allowed, spent = res[0] == 1, res[1]
	}

	// 80% warning: spend (after this request) has reached 80% of the limit.
	warn := limit > 0 && spent*100 >= limit*80

	return BudgetEvaluation{
		Allowed:         allowed,
		SpentTodayUSD:   fromMicro(spent),
		DailyLimitUSD:   dailyLimitUSD,
		IsWarning80:     warn,
		IsCircuitBroken: !allowed,
	}, nil
}
