// Package tokenschema pins the installed personal execution and management storage.
// Organization lifecycle, event replay and privacy stages remain excluded.
package tokenschema

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/jackc/pgx/v5"
	"sort"
	"strings"
)

type identity struct{ name, checksum, downChecksum string }

var installed = map[int64]identity{
	16: {name: "client_readiness", checksum: "c87acabfbc2f0048d50c8d58af1b5a9ecf2ce0cbe9789df90c92192e60a692c5", downChecksum: "d9aeecb29706b1e11db1962815e738de870c1356fb153abe1ee7577a0f85bbe1"},
	17: {name: "usage_admission_indexes", checksum: "fcd9960bf027d6daa6be4c02b109a520398a9515b142f73abd6b9ef5007d09dd", downChecksum: "b61e971e2c5fcff420de457c3da870331ec721abd321f11ae6971de6790108ce"},
	34: {name: "usage_history_groups", checksum: "8f25256da33ce045b00f5f96471dee1169f92047a82fefa42d5485add44a4948", downChecksum: "7ffd599821fb4aa220ca06ef305af84527cbd5b3f27a9fbc9dbadf803f6099c0"},
	1:  {name: "foundation", checksum: "8d9b0e0a9a025079fb0c3549b19fde8d15bba429199756c1fb3696427d5b6530", downChecksum: "694f148cedb118328e90ee64d56d747f98819e13455eb4ab13f1f49ad644b4a9"},
	2:  {name: "token_domain", checksum: "ab5175d41e0627cc5d7a8227ccc055b93ec4cc4be09ed857dcdd7455dfaa1fb1", downChecksum: "53ec51ea986293088294a20c181bb04d339f65d279df2dfeb198a7b0fa25a528"},
	3:  {name: "token_management_audit", checksum: "0c785bf4c2f92f34e283e392b9a552ea34d2f7857975727d2416f5c3b6b4bf3c", downChecksum: "f58dde2cd385347dff75d09b9c479e17dd483cbdfe5cfb414e08213b80116ce2"},
	4:  {name: "event_delivery", checksum: "e75bccd8da580606c7d4800f19267aa930ea08c97cc1b3b58a2d7806d6cea90b", downChecksum: "40473a08b1e36dbab9985b192599af7e38e2a5ec70dab32482c79725e93ecd89"},
	5:  {name: "platform_managed_routes", checksum: "8f883d69bd354f3df610cbec935620147c10d2953a43b1306da9b912532e014c", downChecksum: "58be33664d79125fe58b26eb7d96e16382991dc6b69b0cfb52df9b98d0052e3c"},
	6:  {name: "usage_reconciliation", checksum: "fbe6f72000c39b0eb730a28c297f8336655fa084f48ebf2309388cad96f4e52b", downChecksum: "5fea6a6d5473a6673808b218e42a7942926dad5756bc15d163ab06253f153da4"},
	7:  {name: "gateway_operations", checksum: "edf07b129b426eaca42ff452c77b8f835754071c05263de89ba895fca89c66c0", downChecksum: "e0eeb8bdf985867d519bee2116f58a6ffad0e33bf9fb1fba513f5251ec9caf34"},
	10: {name: "protocol_formula_versions", checksum: "40a37eb1ab122daa40b27ba2554c1dcfda0b066ed174705be1adfc2164fa3734", downChecksum: "8d2e3d5efe27c742dd9580b14e392ae0a7724dd17b93db912916214d3d8c09b4"},
	11: {name: "gateway_adapter_neutrality", checksum: "368c439e88cc3aafd1a173d049f68522c1b83013cb6c3abd65308f141e62fc90", downChecksum: "0ae06c1a023e4afa6982f6a86234c9a54eeec903748ece2f2888f16a46a5b006"},
	12: {name: "model_catalog_lifecycle", checksum: "dda62680bc527e56036b3c8e7d1c05efea45da8aa1f667676708f41e4f3cffe4", downChecksum: "09ac8b24d9163faff505a464608602f0d96637029f8b201c6339cc0ef4b054c2"},
	13: {name: "route_provisioning", checksum: "3a47b52f1258f67781cb123fafa86ec9dea4d2a03707ed57a14cc8f562d4996e", downChecksum: "40f46c41dcfc933a680f9c0d4cc7a0c5a96e85356eec75dd09f431ea93ae0cfa"},
	14: {name: "usage_execution_replay", checksum: "50a85bb91fc6e15c9a0630d1b28f4effd54d8550492a30c8b1cc371f267fa69a", downChecksum: "92cbc8a1f4d521008ec001aaa70f73a6a9c915fc32b13de56b7a6252c7e059a0"},
	15: {name: "usage_cancellation_convergence", checksum: "59c817da9f5f1a7acf8651008e6da260de1ec03fdce86a003cc53a516ed500ed", downChecksum: "3c759ce74e539f5dcf63c0564d8f34487d8f9d2c329ca6db5c9de68825bf7a77"},
	19: {name: "usage_convergence_evidence", checksum: "3844bfb85919334ce4a5a4ed50c9d6129d28ccccef640bc2ab0cafe0007af02f", downChecksum: "6b3714b510e0aa9266b8aa50a80ef8d7ebff53197021de3d98a9b0ddae5ad9d9"},
	20: {name: "manual_review_sla", checksum: "fe03a8e88cb8047884f38bc5ebde9aa711c0cd976a0d344041b097d6b66dd773", downChecksum: "4d489550f7e75cd791f3be53970714d6b1f774d6c120f4fd998b5ac5fb4e9e9e"},
	21: {name: "model_emergency_cancellation", checksum: "a6db7a849a5621d9c4b87caab044e8351a03a62240f6e5fc22f5d4c4a8e922f4", downChecksum: "d0633e0ed04950dc8752d5951f9e73b2d93b4227c0072d876b12d6bde6355fa6"},
	22: {name: "legacy_token_account_quarantine", checksum: "aef0feb65341158062e86c85af27afde85aadc3d614f1e99a2b7862e57c894ef", downChecksum: "b49963078b33d64dbdb1a953618fb8c03b2eaa4c38a3a023dcb0cded88180c13"},
	23: {name: "admin_pagination_snapshots", checksum: "3b7e7ba7e95372e419948ba16824c09813a88da7122874f910ca79ab266155e9", downChecksum: "c8a237b133a4b19b02491169e7210847622907e32f05bc68f53ee115d68c3f7e"},
	24: {name: "token_command_results", checksum: "f79a7a12f69beb6ca360e09ebbec01f028266d7c610ff63b07f0e33f193c263b", downChecksum: "c864e89957ec450f31548143ff418c8dfeeda1e47affca5699e34bf308ec2f9a"},
	25: {name: "stable_routes_health", checksum: "ae91955051536c28168db0ccb0bf8f1c9cf527fd7f0753b949ce02239c8bc86d", downChecksum: "b4dc924c138719812d02ed699daeda78e3ec2ad0d75f57515c6cf2abbf3d762e"},
	26: {name: "credential_isolation", checksum: "a0024f770eae00b2e4a6c20636289d3a3e3a66edf3ddf9fd3c4e213082e6b0cd", downChecksum: "3f5666a049de107580fd596bf58432a0cc0fa0b114359b46e336960f247a71d1"},
	27: {name: "model_lifecycle_plans", checksum: "f2d3ae85f7bd50d1599df649f1a51e667e43560ecc4076e3338571b9ab7480de", downChecksum: "ed21e7ae5bc532243cc632365aee9247a7f89a6f5006d48b05dfdec21b3537aa"},
	28: {name: "operations_overview_indexes", checksum: "36020c29cdcac4f5893cfc0da0b8b12949d0d9b8f7a3cfa955cfb589681b59f9", downChecksum: "38e3222d53a4f0305b8d05f2c110e01aeb9120b0b2ae359af1619b34d8a6ae44"},
	29: {name: "token_account_owner_cap_policy", checksum: "47f091b38bb1fbc78a014e45f51dc31464c791bc8d8de2d40f13c583f864d783", downChecksum: "46d30725765a61f335e2809675181414794a9bf23fc60c75ecce6a3811b5bdd7"},
	30: {name: "credential_probe_unknown_code", checksum: "71374b59f5295e1a0007e1ca9501dcc840248d88afa12c7bbd18ed0c2543fdeb", downChecksum: "ca0978c5309c9df1f336150e9b0435a065bb1074cd5edeab8bbfeffe4453b2dd"},
	31: {name: "token_command_governance_activation", checksum: "d957f8cac9de3566b1e30ab286458f80e78acd2cc9d61917e3367a0c8280ee16", downChecksum: "ffea99c31ae50300e5b055ff867d9c36b203c1db61553f8f6a157f192bcf3d07"},
	32: {name: "token_account_owner_cap_lock", checksum: "cabd925dcab9b2f4e00e35281f807fd4cb2c25f550d84ae0d13da01368e19ca4", downChecksum: "88094ab65e8a81c9aee6b4d9de8b791b869a51c2da83adaef7fcca426eb301f5"},
	33: {name: "model_token_page_overview_indexes", checksum: "a17e2fb2855c376b3a0b41961aa3ad0bc45f4193ebd445d6f4db31c430d3e0d6", downChecksum: "e064ed383496dad953f2d555bfafa501232ad3f50b35648d1bdbe9ec9dc47fd9"},
}

func Ordered(loaded []migrations.Migration) ([]migrations.Migration, error) {
	if len(loaded) != len(installed) {
		return nil, fmt.Errorf("Token management migration package is incomplete or contains another stage")
	}
	result := append([]migrations.Migration(nil), loaded...)
	sort.Slice(result, func(i, j int) bool { return result[i].Version < result[j].Version })
	seen := map[int64]bool{}
	for _, m := range result {
		expected, ok := installed[m.Version]
		if !ok || seen[m.Version] || m.Name != expected.name || m.Checksum != expected.checksum || sqlChecksum(m.UpSQL) != expected.checksum || sqlChecksum(m.DownSQL) != expected.downChecksum {
			return nil, fmt.Errorf("Token management migration identity mismatch")
		}
		seen[m.Version] = true
	}
	return result, nil
}

// Validate the existing ledger before applying anything. In particular an
// F08 privacy/anonymization history cannot be run through the F04 runtime.
func Up(ctx context.Context, c *pgx.Conn, loaded []migrations.Migration) error {
	ordered, err := Ordered(loaded)
	if err != nil {
		return err
	}
	if _, err = c.Exec(ctx, `SELECT pg_advisory_lock(hashtext('pinvou_account_platform_migrations'))`); err != nil {
		return err
	}
	// Balance this wrapper's ledger-validation guard on every exit. The shared
	// migration engine acquires its own session lock, which remains the caller's
	// responsibility to release once; a rejected ledger never reaches that engine.
	defer migrations.Unlock(context.Background(), c)
	var exists bool
	if err = c.QueryRow(ctx, `SELECT to_regclass('schema_migrations') IS NOT NULL`).Scan(&exists); err != nil {
		return err
	}
	if exists {
		rows, err := c.Query(ctx, `SELECT version,name,checksum FROM schema_migrations ORDER BY version`)
		if err != nil {
			return err
		}
		for rows.Next() {
			var version int64
			var name, checksum string
			if err = rows.Scan(&version, &name, &checksum); err != nil {
				rows.Close()
				return err
			}
			expected, ok := installed[version]
			if !ok || name != expected.name || checksum != expected.checksum {
				rows.Close()
				return fmt.Errorf("Token database is outside the installed management stage")
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
	}
	return migrations.UpWithHooks(ctx, c, ordered, migrations.UpHooks{
		BeforeApply: lendOverviewIndexDDL,
		AfterApply:  revokeOverviewIndexDDL,
	})
}

// Original 029 removes runtime schema CREATE; original 033 later creates an
// index on the runtime-owned routing table. Lend only that schema privilege
// inside 033's transaction, then revoke it before recording its ledger row.
// The immutable original SQL and the runtime's steady-state authority stay
// unchanged. A failed statement or hook rolls back the grant with the DDL.
func lendOverviewIndexDDL(ctx context.Context, tx pgx.Tx, m migrations.Migration) error {
	if m.Version != 33 {
		return nil
	}
	var permitted bool
	if err := tx.QueryRow(ctx, `SELECT session_user='token_migrator' AND current_user='token_gateway'
		AND (SELECT pg_get_userbyid(nspowner) FROM pg_namespace WHERE oid=to_regnamespace('public'))='token_schema_owner'
		AND NOT pg_has_role('token_gateway','token_schema_owner','MEMBER')
		AND NOT has_schema_privilege('token_gateway','public','CREATE')`).Scan(&permitted); err != nil {
		return err
	}
	if !permitted {
		return fmt.Errorf("Token overview migration requires the isolated deployment identity")
	}
	_, err := tx.Exec(ctx, `SET ROLE token_schema_owner; GRANT CREATE ON SCHEMA public TO token_gateway`)
	return err
}

func revokeOverviewIndexDDL(ctx context.Context, tx pgx.Tx, m migrations.Migration) error {
	if m.Version != 33 {
		return nil
	}
	_, err := tx.Exec(ctx, `SET ROLE token_schema_owner; REVOKE CREATE ON SCHEMA public FROM token_gateway`)
	return err
}

func sqlChecksum(value string) string {
	sum := sha256.Sum256([]byte(strings.ReplaceAll(value, "\r\n", "\n")))
	return hex.EncodeToString(sum[:])
}
