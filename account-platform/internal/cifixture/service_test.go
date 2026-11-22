package cifixture

import (
	"github.com/jackc/pgx/v5"
	"testing"
)

func TestCoordinatorConfiguration(t *testing.T) {
	c, err := pgx.ParseConfig("postgres://postgres@127.0.0.1:49173/postgres?sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	if !ConfigurationAllowed(c, "49173", "12345", "/var/lib/postgresql/data") {
		t.Fatal("valid service coordinator rejected")
	}
	for _, port := range []string{"", "0", "1023", "049173", "49174", "65536", "49173\n"} {
		if ConfigurationAllowed(c, port, "12345", "/var/lib/postgresql/data") {
			t.Fatal("invalid port admitted")
		}
	}
	for _, system := range []string{"", "0", "abc", "12345\n"} {
		if ConfigurationAllowed(c, "49173", system, "/var/lib/postgresql/data") {
			t.Fatal("invalid system identifier admitted")
		}
	}
	runtime := c.Copy()
	runtime.User = "billing_service"
	if ConfigurationAllowed(runtime, "49173", "12345", "/var/lib/postgresql/data") {
		t.Fatal("runtime role received coordinator admission")
	}
	owned := c.Copy()
	owned.Database = "owned_fixture_uuid"
	if !ConfigurationAllowed(owned, "49173", "12345", "/var/lib/postgresql/data") {
		t.Fatal("postgres coordinator of its already owned UUID database rejected")
	}
}
