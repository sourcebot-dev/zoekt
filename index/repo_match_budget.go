package index

import (
	"context"
	"sync"
	"sync/atomic"

	"github.com/sourcegraph/zoekt"
)

type repoMatchBudgetContextKey struct{}

type repoMatchBudget struct {
	limit     int
	remaining sync.Map // repoBudgetKey -> *atomic.Int64
}

type repoBudgetKey struct {
	tenantID int
	name     string
}

// WithRepoMatchBudget shares one search's per-repository budget across its
// shards. Index searches without this context enforce the limit locally.
func WithRepoMatchBudget(ctx context.Context, limit int) context.Context {
	if limit <= 0 {
		return ctx
	}
	return context.WithValue(ctx, repoMatchBudgetContextKey{}, &repoMatchBudget{limit: limit})
}

func (b *repoMatchBudget) forRepo(repo *zoekt.Repository) *atomic.Int64 {
	key := repoBudgetKey{tenantID: repo.TenantID, name: repo.Name}
	if remaining, ok := b.remaining.Load(key); ok {
		return remaining.(*atomic.Int64)
	}

	initial := new(atomic.Int64)
	initial.Store(int64(b.limit))
	remaining, _ := b.remaining.LoadOrStore(key, initial)
	return remaining.(*atomic.Int64)
}

func claimRepoMatches(remaining *atomic.Int64, matches int) int {
	for {
		left := remaining.Load()
		allowed := min(left, int64(matches))
		if allowed == 0 || remaining.CompareAndSwap(left, left-allowed) {
			return int(allowed)
		}
	}
}
