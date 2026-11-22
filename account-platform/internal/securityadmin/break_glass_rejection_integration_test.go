package securityadmin

import (
	"testing"

	"github.com/Pinvou/pinvou3/account-platform/internal/identityadapter"
)

func TestRetainedSecurityBreakGlassPasswordRejectionBoundary(t *testing.T) {
	const policy = `{"error":"invalidPasswordHistoryMessage","error_description":"Password policy rejected the candidate"}`
	for _, test := range []struct {
		name, body            string
		status                int
		unchanged, disconnect bool
	}{
		{name: "confirmed-policy", status: 400, body: policy, unchanged: true},
		{name: "generic-400", status: 400, body: `{"error":"storageFailure","error_description":"failed"}`},
		{name: "malformed-policy", status: 400, body: policy + `{}`},
		{name: "forbidden", status: 403, body: policy},
		{name: "dependency-failure", status: 503, body: policy},
		{name: "password-write-lost-ack", disconnect: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := retainedSecuritySetup(t)
			state := retainedRealIdentity(t, f)
			f.activate(t)
			request := retainedSignedRecovery(t, f)
			facts := func() string {
				var result string
				if err := f.connection.QueryRow(f.ctx, `SELECT json_build_object(
 'status',u.status,'version',u.version,'membership_version',u.membership_version,
 'roles',(SELECT json_agg(r ORDER BY r.id) FROM role_assignments r WHERE r.user_id=u.id),
 'wallet',(SELECT json_agg(j ORDER BY j.user_id) FROM wallet_provisioning_jobs j WHERE j.user_id=u.id),
 'operations',(SELECT count(*) FROM break_glass_operations),
 'audit',(SELECT count(*) FROM audit_logs),
 'alerts',(SELECT count(*) FROM security_alerts))::text FROM users u WHERE username='admin'`).Scan(&result); err != nil {
					t.Fatal(err)
				}
				return result
			}
			before := facts()
			state.mu.Lock()
			state.resetFailureStatus, state.resetFailureBody, state.resetDisconnect = test.status, test.body, test.disconnect
			attempts, resets, enables, disables, logouts := state.resetAttempts, state.resets, state.enables, state.disables, state.logouts
			state.mu.Unlock()
			err := f.manager.BreakGlass(f.ctx, request)
			if err == nil || identityadapter.BootstrapActivationUnchanged(err) != test.unchanged {
				t.Fatalf("unexpected rejection proof=%t err=%v", identityadapter.BootstrapActivationUnchanged(err), err)
			}
			if after := facts(); after != before {
				t.Fatalf("rejected recovery changed durable Account facts\nbefore=%s\nafter=%s", before, after)
			}
			state.mu.Lock()
			wantDisable, wantLogout, wantResets := 1, 1, 0
			if test.unchanged {
				wantDisable, wantLogout = 0, 0
			}
			if test.disconnect {
				wantResets = 1
			}
			if state.enabled != test.unchanged || state.resetAttempts-attempts != 1 || state.resets-resets != wantResets || state.enables-enables != 0 || state.disables-disables != wantDisable || state.logouts-logouts != wantLogout {
				t.Fatalf("enabled=%t reset_attempts=%d resets=%d enables=%d disables=%d logouts=%d", state.enabled, state.resetAttempts-attempts, state.resets-resets, state.enables-enables, state.disables-disables, state.logouts-logouts)
			}
			state.resetFailureStatus, state.resetFailureBody, state.resetDisconnect = 0, "", false
			state.mu.Unlock()
			// The failed transaction must not consume the signed operator credential.
			if err := f.manager.BreakGlass(f.ctx, request); err != nil {
				t.Fatalf("recovery retry failed: %v", err)
			}
			var operations int
			if err := f.connection.QueryRow(f.ctx, `SELECT count(*) FROM break_glass_operations`).Scan(&operations); err != nil || operations != 1 {
				t.Fatalf("retry receipt count=%d err=%v", operations, err)
			}
		})
	}
}
