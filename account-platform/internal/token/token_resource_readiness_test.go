package token

import (
	"errors"
	"testing"
)

func TestBuildTokenResourceReadiness(t *testing.T) {
	tests := []struct {
		name             string
		authorization    bool
		pagination       bool
		paginationErr    error
		activation       TokenCommandGovernanceActivation
		activationErr    error
		wantMode         string
		wantRecovery     bool
		wantReason       string
		wantGovernance   string
	}{
		{name: "authorization version missing", wantMode: tokenResourceModeBlocked, wantReason: "AUTHORIZATION_VERSION_INVALID", wantGovernance: "UNAVAILABLE"},
		{name: "bounded legacy when pagination is disabled", authorization: true, activation: TokenCommandGovernanceActivation{State: "PRE_ACTIVATION"}, wantMode: tokenResourceModeLegacy, wantReason: "PAGINATION_DISABLED", wantGovernance: "PRE_ACTIVATION"},
		{name: "pagination control unavailable", authorization: true, paginationErr: errors.New("database unavailable"), wantMode: tokenResourceModeBlocked, wantReason: "PAGINATION_UNAVAILABLE", wantGovernance: "UNAVAILABLE"},
		{name: "governance unavailable", authorization: true, pagination: true, activationErr: errors.New("governance unavailable"), wantMode: tokenResourceModeSnapshot, wantReason: "GOVERNANCE_UNAVAILABLE", wantGovernance: "UNAVAILABLE"},
		{name: "pre activation recovery", authorization: true, pagination: true, activation: TokenCommandGovernanceActivation{State: "PRE_ACTIVATION"}, wantMode: tokenResourceModeSnapshot, wantRecovery: true, wantGovernance: "PRE_ACTIVATION"},
		{name: "active recovery", authorization: true, pagination: true, activation: TokenCommandGovernanceActivation{State: "ACTIVE", Epoch: 7}, wantMode: tokenResourceModeSnapshot, wantRecovery: true, wantGovernance: "ACTIVE"},
		{name: "invalid active epoch", authorization: true, pagination: true, activation: TokenCommandGovernanceActivation{State: "ACTIVE"}, wantMode: tokenResourceModeSnapshot, wantReason: "GOVERNANCE_INACTIVE", wantGovernance: "ACTIVE"},
		{name: "unknown governance state is closed", authorization: true, pagination: true, activation: TokenCommandGovernanceActivation{State: "CORRUPT", Epoch: 9}, wantMode: tokenResourceModeSnapshot, wantReason: "GOVERNANCE_UNAVAILABLE", wantGovernance: "UNAVAILABLE"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			projection := buildTokenResourceReadiness(test.authorization, test.pagination, test.paginationErr, test.activation, test.activationErr)
			if projection.TokenAccountsMode != test.wantMode || projection.RecoveryQueueAvailable != test.wantRecovery || projection.RecoveryUnavailableReason != test.wantReason || projection.GovernanceState != test.wantGovernance {
				t.Fatalf("projection=%+v", projection)
			}
		})
	}
}
