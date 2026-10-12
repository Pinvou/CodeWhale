package billing

import (
	"errors"
	"math"
	"testing"

	"github.com/google/uuid"
)

func TestMemberHoldIdentityReservedFacts(t *testing.T) {
	org, member, wallet, pack, component := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	version := int64(1)
	identity := memberHoldIdentity{walletID: wallet, walletOwnerID: org, walletOwnerType: "ORGANIZATION", organizationID: &org, memberUserID: &member, membershipVersion: &version, maximum: 100}
	c := memberHoldComponent{packageID: pack, componentPackageID: pack, componentID: component, walletID: wallet, reserved: 100}
	a := memberHoldGrantAllocation{organizationID: org, memberUserID: member, packageID: pack, componentPackageID: pack, componentID: component, grantComponentID: component, walletID: wallet, componentType: "BASE", grantComponentType: "BASE", reserved: 100}
	t.Run("complete-original-reservation", func(t *testing.T) {
		if err := checkMemberHoldIdentity(identity, []memberHoldComponent{c}, []memberHoldGrantAllocation{a}, 0); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("zero-budget-without-allocations", func(t *testing.T) {
		h := identity
		h.maximum = 0
		if err := checkMemberHoldIdentity(h, nil, nil, 0); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("split-grants-same-component", func(t *testing.T) {
		one, two := a, a
		one.reserved = 30
		two.reserved = 70
		if err := checkMemberHoldIdentity(identity, []memberHoldComponent{c}, []memberHoldGrantAllocation{one, two}, 0); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("extended-budget", func(t *testing.T) {
		cc, aa := c, a
		cc.reserved = 120
		aa.reserved = 120
		if err := checkMemberHoldIdentity(identity, []memberHoldComponent{cc}, []memberHoldGrantAllocation{aa}, 20); err != nil {
			t.Fatal(err)
		}
	})
	for _, tc := range []struct {
		name   string
		mutate func(*memberHoldIdentity, *memberHoldComponent, *memberHoldGrantAllocation)
	}{
		{"personal-synthetic-member", func(h *memberHoldIdentity, _ *memberHoldComponent, _ *memberHoldGrantAllocation) {
			h.walletOwnerType = "PERSONAL"
			h.organizationID = nil
			h.memberUserID = nil
			h.membershipVersion = nil
		}},
		{"partial-scope", func(h *memberHoldIdentity, _ *memberHoldComponent, _ *memberHoldGrantAllocation) {
			h.memberUserID = nil
		}},
		{"zero-snapshot-version", func(h *memberHoldIdentity, _ *memberHoldComponent, _ *memberHoldGrantAllocation) {
			v := int64(0)
			h.membershipVersion = &v
		}},
		{"foreign-wallet-owner", func(h *memberHoldIdentity, _ *memberHoldComponent, _ *memberHoldGrantAllocation) {
			h.walletOwnerID = uuid.New()
		}},
		{"foreign-grant-org", func(_ *memberHoldIdentity, _ *memberHoldComponent, a *memberHoldGrantAllocation) {
			a.organizationID = uuid.New()
		}},
		{"foreign-grant-member", func(_ *memberHoldIdentity, _ *memberHoldComponent, a *memberHoldGrantAllocation) {
			a.memberUserID = uuid.New()
		}},
		{"foreign-component", func(_ *memberHoldIdentity, _ *memberHoldComponent, a *memberHoldGrantAllocation) {
			a.grantComponentID = uuid.New()
		}},
		{"foreign-package", func(_ *memberHoldIdentity, _ *memberHoldComponent, a *memberHoldGrantAllocation) {
			a.packageID = uuid.New()
		}},
		{"foreign-package-wallet", func(_ *memberHoldIdentity, _ *memberHoldComponent, a *memberHoldGrantAllocation) {
			a.walletID = uuid.New()
		}},
		{"wrong-component-type", func(_ *memberHoldIdentity, _ *memberHoldComponent, a *memberHoldGrantAllocation) {
			a.grantComponentType = "BONUS"
		}},
		{"generic-package-mismatch", func(_ *memberHoldIdentity, c *memberHoldComponent, _ *memberHoldGrantAllocation) {
			c.componentPackageID = uuid.New()
		}},
		{"generic-budget-short", func(_ *memberHoldIdentity, c *memberHoldComponent, _ *memberHoldGrantAllocation) { c.reserved = 99 }},
		{"member-budget-short", func(_ *memberHoldIdentity, _ *memberHoldComponent, a *memberHoldGrantAllocation) { a.reserved = 99 }},
		{"member-budget-excess", func(_ *memberHoldIdentity, _ *memberHoldComponent, a *memberHoldGrantAllocation) { a.reserved = 101 }},
		{"already-settled-member", func(_ *memberHoldIdentity, _ *memberHoldComponent, a *memberHoldGrantAllocation) { a.settled = 1 }},
		{"already-released-component", func(_ *memberHoldIdentity, c *memberHoldComponent, _ *memberHoldGrantAllocation) { c.released = 1 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, cc, aa := identity, c, a
			tc.mutate(&h, &cc, &aa)
			if err := checkMemberHoldIdentity(h, []memberHoldComponent{cc}, []memberHoldGrantAllocation{aa}, 0); !errors.Is(err, ErrConflict) {
				t.Fatalf("invalid reserved facts accepted: %v", err)
			}
		})
	}
	t.Run("missing-member-allocation", func(t *testing.T) {
		if err := checkMemberHoldIdentity(identity, []memberHoldComponent{c}, nil, 0); !errors.Is(err, ErrConflict) {
			t.Fatal(err)
		}
	})
	t.Run("missing-generic-allocation", func(t *testing.T) {
		if err := checkMemberHoldIdentity(identity, nil, []memberHoldGrantAllocation{a}, 0); !errors.Is(err, ErrConflict) {
			t.Fatal(err)
		}
	})
	t.Run("overflowing-extension", func(t *testing.T) {
		h := identity
		h.maximum = math.MaxInt64
		if err := checkMemberHoldIdentity(h, nil, nil, 1); !errors.Is(err, ErrConflict) {
			t.Fatal(err)
		}
	})
}
