package app

import (
	"context"
	"fmt"

	"go.mau.fi/whatsmeow/appstate"
	"go.mau.fi/whatsmeow/types"
)

// ReadLabelMutations returns the raw label mutations from the regular app state
// collection.
//
// Labels live only in app state, and this account class cannot read them
// through the normal paths: a full sync and the primary-device recovery
// snapshot both fail verification when the patch history carries an LTHash
// mismatch. Reading the snapshot without MAC validation is the remaining route.
// The result is untrusted and is only ever displayed, never written back as
// canonical state.
func (a *App) ReadLabelMutations(ctx context.Context) ([]appstate.Mutation, error) {
	return a.wa.FetchAppStateSnapshotUnverified(ctx, string(appstate.WAPatchRegular))
}

// EditLabel creates, renames, recolors, or deletes a label.
//
// Labels live in the regular collection, and the send is rejected with a 409
// conflict whenever the local version lags the server head. On an account
// whose regular chain fails verification that lag is the normal state: every
// patch the phone writes fails to decode locally and leaves the row one behind.
// The write therefore takes the same pre-write sync as the chat flags, which
// detects the lag and restores the head from a primary-device snapshot before
// the send. Labels keep no local copy, so unlike the chat flags they record no
// replay debt after the send.
func (a *App) EditLabel(ctx context.Context, labelID, name string, color int32, deleted bool) error {
	release, err := a.beginChatStateWrite(ctx, appstate.WAPatchRegular)
	if err != nil {
		return err
	}
	defer release()
	return a.wa.EditLabel(ctx, labelID, name, color, deleted)
}

// LabelChat attaches or detaches one label on one chat. See EditLabel for the
// pre-write sync.
//
// The mutation goes to the LID form of the chat. See labelChatTargets.
func (a *App) LabelChat(ctx context.Context, jid types.JID, labelID string, labeled bool) error {
	release, err := a.beginChatStateWrite(ctx, appstate.WAPatchRegular)
	if err != nil {
		return err
	}
	defer release()
	for _, target := range a.labelChatTargets(ctx, jid, labelID, labeled) {
		if err := a.wa.LabelChat(ctx, target, labelID, labeled); err != nil {
			return err
		}
	}
	return nil
}

// labelChatTargets returns the JIDs that carry the label mutation for one chat.
//
// WhatsApp keys a label membership on the contact's LID identity, not on the
// phone JID. A membership written on the phone JID does appear in the label
// list, but the chat menu on the phone then offers "Add to list" instead of
// "Change to list", and a removal made on the phone does not stick, because the
// phone-form entry survives it. An attach therefore writes the LID form
// whenever the session knows one.
//
// A detach writes both forms. An entry written on the phone JID before this
// rule must still be removable, and a delete mutation for an absent key is
// harmless.
//
// Groups and targets that already carry a LID pass through unchanged.
func (a *App) labelChatTargets(ctx context.Context, jid types.JID, labelID string, labeled bool) []types.JID {
	if jid.Server != types.DefaultUserServer {
		return []types.JID{jid}
	}
	pn := jid.ToNonAD()
	lid := a.wa.ResolvePNToLID(ctx, pn)
	if lid.Server != types.HiddenUserServer || lid.IsEmpty() {
		a.emitWarning("label_chat_no_lid",
			fmt.Sprintf("warning: no LID known for %s; writing the label on the phone JID", pn),
			map[string]any{"chat": pn.String(), "label_id": labelID})
		return []types.JID{pn}
	}
	if labeled {
		return []types.JID{lid}
	}
	return []types.JID{lid, pn}
}
