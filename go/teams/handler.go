package teams

import (
	"context"
	"fmt"

	"github.com/keybase/client/go/engine"
	"github.com/keybase/client/go/libkb"
	"github.com/keybase/client/go/protocol/keybase1"
)

func HandleRotateRequest(ctx context.Context, g *libkb.GlobalContext, teamID keybase1.TeamID, generation keybase1.PerTeamKeyGeneration) (err error) {

	ctx = libkb.WithLogTag(ctx, "CLKR")
	defer g.CTrace(ctx, fmt.Sprintf("HandleRotateRequest(%s,%d)", teamID, generation), func() error { return err })()

	team, err := Load(ctx, g, keybase1.LoadTeamArg{
		ID:          teamID,
		ForceRepoll: true,
	})
	if err != nil {
		return err
	}

	if team.Generation() > generation {
		g.Log.CDebugf(ctx, "current team generation %d > team.clkr generation %d, not rotating", team.Generation(), generation)
		return nil
	}

	g.Log.CDebugf(ctx, "rotating team %s (%s)", team.Name(), teamID)
	if err := team.Rotate(ctx); err != nil {
		g.Log.CDebugf(ctx, "rotating team %s (%s) error: %s", team.Name(), teamID, err)
		return err
	}

	g.Log.CDebugf(ctx, "sucess rotating team %s (%s)", team.Name(), teamID)
	return nil
}

func reloadLocal(ctx context.Context, g *libkb.GlobalContext, row keybase1.TeamChangeRow, change keybase1.TeamChangeSet) error {
	if change.Renamed {
		// This force reloads the team as a side effect
		return g.GetTeamLoader().NotifyTeamRename(ctx, row.Id, row.Name)
	}

	_, err := Load(ctx, g, keybase1.LoadTeamArg{
		ID:          row.Id,
		ForceRepoll: true,
	})
	return err
}

func handleChangeSingle(ctx context.Context, g *libkb.GlobalContext, row keybase1.TeamChangeRow, change keybase1.TeamChangeSet) (err error) {
	change.KeyRotated = row.KeyRotated
	change.MembershipChanged = row.MembershipChanged

	defer g.CTrace(ctx, fmt.Sprintf("team.handleChangeSingle(%+v, %+v)", row, change), func() error { return err })()

	if err = reloadLocal(ctx, g, row, change); err != nil {
		return err
	}
	g.NotifyRouter.HandleTeamChanged(ctx, row.Id, row.Name, row.LatestSeqno, change)
	return nil
}

func HandleChangeNotification(ctx context.Context, g *libkb.GlobalContext, rows []keybase1.TeamChangeRow, changes keybase1.TeamChangeSet) (err error) {
	ctx = libkb.WithLogTag(ctx, "CLKR")
	defer g.CTrace(ctx, "HandleChangeNotification", func() error { return err })()
	for _, row := range rows {
		if err := handleChangeSingle(ctx, g, row, changes); err != nil {
			return err
		}
	}
	return nil
}

func HandleDeleteNotification(ctx context.Context, g *libkb.GlobalContext, rows []keybase1.TeamChangeRow) error {
	for _, row := range rows {
		g.NotifyRouter.HandleTeamDeleted(ctx, row.Id)
	}
	return nil
}

func HandleSBSRequest(ctx context.Context, g *libkb.GlobalContext, msg keybase1.TeamSBSMsg) (err error) {
	ctx = libkb.WithLogTag(ctx, "CLKR")
	defer g.CTrace(ctx, "HandleSBSRequest", func() error { return err })()
	for _, invitee := range msg.Invitees {
		if err := handleSBSSingle(ctx, g, msg.TeamID, invitee); err != nil {
			return err
		}
	}
	return nil
}

func handleSBSSingle(ctx context.Context, g *libkb.GlobalContext, teamID keybase1.TeamID, untrustedInviteeFromGregor keybase1.TeamInvitee) (err error) {
	defer g.CTrace(ctx, fmt.Sprintf("team.handleSBSSingle(teamID: %v, invitee: %+v)", teamID, untrustedInviteeFromGregor), func() error { return err })()
	uv := NewUserVersion(untrustedInviteeFromGregor.Uid, untrustedInviteeFromGregor.EldestSeqno)
	req, err := reqFromRole(uv, untrustedInviteeFromGregor.Role)
	if err != nil {
		return err
	}
	req.CompletedInvites = make(map[keybase1.TeamInviteID]keybase1.UserVersionPercentForm)
	req.CompletedInvites[untrustedInviteeFromGregor.InviteID] = uv.PercentForm()

	team, err := Load(ctx, g, keybase1.LoadTeamArg{
		ID:          teamID,
		ForceRepoll: true,
	})
	if err != nil {
		return err
	}

	// verify the invite info:

	// find the invite in the team chain
	invite, found := team.chain().FindActiveInviteByID(untrustedInviteeFromGregor.InviteID)
	if !found {
		return libkb.NotFoundError{}
	}
	category, err := invite.Type.C()
	if err != nil {
		return err
	}
	switch category {
	case keybase1.TeamInviteCategory_SBS:
		//  resolve assertion in link (with uid in invite msg)
		ityp, err := invite.Type.String()
		if err != nil {
			return err
		}
		assertion := fmt.Sprintf("%s@%s+uid:%s", string(invite.Name), ityp, untrustedInviteeFromGregor.Uid)

		arg := keybase1.Identify2Arg{
			UserAssertion:    assertion,
			UseDelegateUI:    false,
			Reason:           keybase1.IdentifyReason{Reason: "process team invite"},
			CanSuppressUI:    true,
			IdentifyBehavior: keybase1.TLFIdentifyBehavior_CHAT_GUI,
		}
		ectx := &engine.Context{
			NetContext: ctx,
		}
		eng := engine.NewResolveThenIdentify2(g, &arg)
		if err := engine.RunEngine(eng, ectx); err != nil {
			return err
		}
	case keybase1.TeamInviteCategory_EMAIL:
		// nothing to verify, need to trust the server
	case keybase1.TeamInviteCategory_KEYBASE:
		if err := assertCanAcceptKeybaseInvite(ctx, g, untrustedInviteeFromGregor, invite); err != nil {
			return err
		}
	default:
		return fmt.Errorf("no verification implemented for invite category %s (%+v)", category, invite)
	}

	g.Log.CDebugf(ctx, "checks passed, proceeding with team.ChangeMembership, req = %+v", req)

	return team.ChangeMembership(ctx, req)
}

func assertCanAcceptKeybaseInvite(ctx context.Context, g *libkb.GlobalContext, untrustedInviteeFromGregor keybase1.TeamInvitee, chainInvite keybase1.TeamInvite) error {
	chainUV, err := chainInvite.KeybaseUserVersion()
	if err != nil {
		return err
	}
	if chainUV.Uid.NotEqual(untrustedInviteeFromGregor.Uid) {
		return fmt.Errorf("chain keybase invite link uid %s does not match uid %s in team.sbs message", chainUV.Uid, untrustedInviteeFromGregor.Uid)
	}

	if chainUV.EldestSeqno.Eq(untrustedInviteeFromGregor.EldestSeqno) {
		return nil
	}

	if chainUV.EldestSeqno == 0 {
		g.Log.CDebugf(ctx, "team.sbs invitee eldest seqno: %d, allowing it to take the invite for eldest seqno 0 (reset account)", untrustedInviteeFromGregor.EldestSeqno)
		return nil
	}

	return fmt.Errorf("chain keybase invite link eldest seqno %d does not match eldest seqno %d in team.sbs message", chainUV.EldestSeqno, untrustedInviteeFromGregor.EldestSeqno)
}
