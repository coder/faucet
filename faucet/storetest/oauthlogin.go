package storetest

import (
	"testing"

	"github.com/coder/faucet/faucet/oauth"
)

// loginSubtests is the contract of oauth.LoginStore, one behavior per name.
var loginSubtests = []struct {
	name string
	run  func(t *testing.T, s oauth.LoginStore)
}{
	// Authorization requests.
	{"AuthRequestStartsPendingAndIsFoundByNoHashYet", nil},
	{"InsertAuthRequestRefusesASecondRequestWithTheSameID", nil},
	{"InsertAuthRequestValidatesInput", nil},
	{"ConsentSetsTheGitHubStateOfAPendingRequestWithThatBrowser", nil},
	{"ConsentRefusesAnotherBrowserAnExpiredRequestAndAnUnknownID", nil},
	{"ALaterConsentReplacesTheGitHubState", nil},
	{"ExpireEndsOnlyAPendingRequest", nil},
	{"ApproveStoresTheConnectionAndTheCodeInOneStep", nil},
	{"ApproveRefusesAnOutdatedGitHubStateAnExpiredRequestAndASecondApproval", nil},
	{"ARefusedApprovalLeavesTheConnectionAsItWas", nil},
	{"ApproveReplacesTheTokensOfAUserWhoLogsInAgain", nil},
	{"RedeemMakesTheSessionAndItsFirstAccessTokenFromTheRequest", nil},
	{"RedeemRefusesAnExpiredCodeAnUnknownCodeAndASecondUse", nil},
	{"OnlyOneOfManyRedemptionsAtOnceMakesASession", nil},
	{"PurgeDeletesTheRequestsThatExpiredBeforeTheTimeAndKeepsTheirSessions", nil},
	// Sessions.
	{"SessionIsFoundByItsCurrentAndByItsPreviousRefreshToken", nil},
	{"RotateReplacesTheRefreshTokenAndStoresTheNextAccessToken", nil},
	{"RotateRefusesARotatedOutTokenAnEndedSessionAndAnUnknownToken", nil},
	{"OnlyOneOfManyRotationsAtOnceWins", nil},
	{"EndSessionKeepsTheFirstTime", nil},
	// Connections.
	{"ConnectionOfAnUnknownIDIsUnknown", nil},
	{"RefreshConnectionKeepsReplacesOrEndsTheSessions", nil},
	{"RefreshConnectionEndsOnlyTheLiveSessionsOfItsConnection", nil},
	{"RefreshConnectionChangesNothingWhenItsCallbackFailsOrItsChangeIsNotValid", nil},
	{"ASecondRefreshOfAConnectionWaitsAndSeesWhatTheFirstStored", nil},
	{"AnApprovalWaitsForARefreshOfItsConnection", nil},
	{"LoginRowsShareNoSliceWithTheirCaller", nil},
}

// runOAuthLogin runs the oauth.LoginStore contract. A behavior whose test
// is not written yet is skipped.
func runOAuthLogin(t *testing.T, newStore func(t *testing.T) oauth.LoginStore) {
	for _, st := range loginSubtests {
		t.Run(st.name, func(t *testing.T) {
			t.Parallel()
			if st.run == nil {
				t.Skip("not written yet")
			}
			st.run(t, newStore(t))
		})
	}
}
