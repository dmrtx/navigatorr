package store

import (
	"path/filepath"
	"testing"
)

// A database already at schema 21 from an earlier build of this change, which
// created only action_replace_when_done there, gets the later tables.
func TestADatabaseAtTwentyOneGetsTheReplacementTables(t *testing.T) {
	path := filepath.Join(t.TempDir(), "at21.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`DROP TABLE replacement_approvals`, `DROP TABLE auto_replace_pending`, `DROP TABLE replace_receipts`, `DROP TABLE replace_settings`,
		`DELETE FROM schema_version WHERE version > 21`,
	} {
		if _, err := st.db.Exec(stmt); err != nil {
			t.Fatal(stmt, err)
		}
	}
	st.Close()
	if st, err = Open(path); err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.GrantReplacementApproval("file", "reviewed", "promote"); err != nil {
		t.Fatal(err)
	}
	if err := st.AddAutoReplacePending("file"); err != nil {
		t.Fatal(err)
	}
	if err := st.PutReplaceReceipt("key", ReplaceReceipt{Request: "{}", Status: 202, Response: "{}"}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetReplaceWhenDoneDefault(true); err != nil {
		t.Fatal(err)
	}
	// Opening again over tables that already exist is harmless.
	st.Close()
	if st, err = Open(path); err != nil {
		t.Fatal(err)
	}
	if on, _ := st.ReplaceWhenDoneDefault(); !on {
		t.Fatal("default lost")
	}
}
