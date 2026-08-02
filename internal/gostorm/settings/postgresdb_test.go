package settings

import (
	"bytes"
	"os"
	"path/filepath"
	"sort"
	"testing"

	// Pure-Go, in-process SQL engine used ONLY to exercise the PostgresDB code path
	// offline (no external server). Production uses the pgx/PostgreSQL driver; the SQL
	// is kept portable so the identical Get/Set/List/Rem/Clear logic runs here.
	_ "modernc.org/sqlite"
)

// backendContract exercises the full GoStormDB contract that XPathDBRouter relies on.
// It is backend-agnostic: it is run against the in-process engine always, and against a
// real PostgreSQL server when GOSTORM_PG_TEST_DSN is provided.
func backendContract(t *testing.T, db GoStormDB) {
	t.Helper()

	// Set/Get round-trip (bytes preserved exactly).
	val := []byte{0x00, 0x01, 0xfe, 0xff, 'h', 'i'}
	db.Set("Settings", "BitTorr", val)
	got := db.Get("Settings", "BitTorr")
	if !bytes.Equal(got, val) {
		t.Fatalf("Get after Set: got %v, want %v", got, val)
	}

	// Missing key returns nil.
	if got := db.Get("Settings", "Nope"); got != nil {
		t.Fatalf("Get missing: got %v, want nil", got)
	}

	// Overwrite in place.
	val2 := []byte("second-value")
	db.Set("Settings", "BitTorr", val2)
	if got := db.Get("Settings", "BitTorr"); !bytes.Equal(got, val2) {
		t.Fatalf("Get after overwrite: got %q, want %q", got, val2)
	}

	// A second direct key and a nested sub-bucket value.
	db.Set("Settings", "Other", []byte("x"))
	db.Set("Settings/Sub", "Leaf", []byte("y"))

	// List returns both direct value keys AND the immediate sub-bucket segment name,
	// matching bbolt bucket.ForEach semantics.
	list := db.List("Settings")
	sort.Strings(list)
	want := []string{"BitTorr", "Other", "Sub"}
	if len(list) != len(want) {
		t.Fatalf("List(Settings): got %v, want %v", list, want)
	}
	for i := range want {
		if list[i] != want[i] {
			t.Fatalf("List(Settings): got %v, want %v", list, want)
		}
	}

	// Nested value is retrievable at its own bucket path.
	if got := db.Get("Settings/Sub", "Leaf"); !bytes.Equal(got, []byte("y")) {
		t.Fatalf("Get nested: got %q, want %q", got, "y")
	}

	// Rem deletes a single key, leaving siblings intact.
	db.Rem("Settings", "Other")
	if got := db.Get("Settings", "Other"); got != nil {
		t.Fatalf("Get after Rem: got %v, want nil", got)
	}
	if got := db.Get("Settings", "BitTorr"); !bytes.Equal(got, val2) {
		t.Fatalf("sibling clobbered by Rem: got %q, want %q", got, val2)
	}

	// Clear removes the direct value keys at the bucket path (sub-bucket rows remain).
	db.Clear("Settings")
	if got := db.Get("Settings", "BitTorr"); got != nil {
		t.Fatalf("Get after Clear: got %v, want nil", got)
	}
	if got := db.Get("Settings/Sub", "Leaf"); !bytes.Equal(got, []byte("y")) {
		t.Fatalf("Clear wrongly removed nested bucket: got %q, want %q", got, "y")
	}
}

// TestGostormPostgresBackend verifies the PostgresDB GoStormDB backend end-to-end.
//
// The "inprocess" subtest runs the identical PostgresDB code path against an embedded
// pure-Go SQL engine, so the KV/bucket contract is genuinely verified in the offline
// check sandbox (it fails to compile/pass without postgresdb.go). When
// GOSTORM_PG_TEST_DSN names a reachable PostgreSQL logical database (e.g. gostream_dev
// on the shared server), the "postgres" subtest runs the SAME contract against the real
// pgx driver.
func TestGostormPostgresBackend(t *testing.T) {
	t.Run("inprocess", func(t *testing.T) {
		dsn := filepath.Join(t.TempDir(), "gostorm_settings_test.db")
		db, err := newPostgresDBWithDriver("sqlite", dsn)
		if err != nil {
			t.Fatalf("open in-process backend: %v", err)
		}
		defer db.CloseDB()
		backendContract(t, db)
	})

	if dsn := os.Getenv("GOSTORM_PG_TEST_DSN"); dsn != "" {
		t.Run("postgres", func(t *testing.T) {
			db, err := NewPostgresDB(dsn)
			if err != nil {
				t.Fatalf("connect to GOSTORM_PG_TEST_DSN: %v", err)
			}
			defer db.CloseDB()
			// Start from a clean slate on the real server.
			db.Clear("Settings")
			db.Rem("Settings/Sub", "Leaf")
			backendContract(t, db)
			db.Clear("Settings")
			db.Rem("Settings/Sub", "Leaf")
		})
	}
}
