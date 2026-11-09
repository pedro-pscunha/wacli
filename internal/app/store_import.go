package app

import (
	"context"
	"fmt"

	"github.com/openclaw/wacli/internal/store"
)

// ImportStore copies into this store the history of another store of the same account (store.DB.ImportFrom). It
// resolves hidden-user addresses with this account's own LID map, read from session.db without connecting, so a copied
// row lands under the address that sync would give it and is never a second copy of a message this store holds under
// the phone number.
func (a *App) ImportStore(ctx context.Context, source string, dryRun bool) (store.ImportResult, error) {
	if err := a.OpenWA(); err != nil {
		return store.ImportResult{}, err
	}
	// Without a linked device session.db holds no LID map, and every hidden-user address would stay unresolved
	// without a word: a wrong --store, or a logged-out store as the target.
	if a.wa.LinkedJID() == "" {
		return store.ImportResult{}, fmt.Errorf("this store has no linked device, so its LID map is empty: import into the live store of the account")
	}
	return a.db.ImportFrom(ctx, source, store.ImportOptions{
		DryRun:    dryRun,
		Canonical: func(jid string) string { return a.canonicalStoreJIDString(ctx, jid) },
	})
}
