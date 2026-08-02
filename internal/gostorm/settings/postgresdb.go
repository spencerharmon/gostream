package settings

import (
	"database/sql"
	"fmt"
	"os"
	"strings"

	"gostream/internal/gostorm/log"

	// database/sql driver for PostgreSQL (production backend).
	_ "github.com/jackc/pgx/v5/stdlib"
)

// PostgresDB is a GoStormDB backend that stores gostorm settings/config state in a
// PostgreSQL logical database (gostream_prod / gostream_dev on the shared Postgres
// server) instead of the per-color config.db bbolt file. It implements the exact same
// GoStormDB contract (Get/Set/List/Rem/Clear/CloseDB) as the bbolt (TDB) and JSON
// backends, so it drops straight into the existing XPathDBRouter — a color flip then
// needs no file-level DB sync because the state lives in shared Postgres.
//
// The hierarchical bbolt "bucket path / name" model is flattened onto a single table
// keyed by (xpath, name); nested buckets are recovered from xpath prefixes so List()
// still returns both direct value keys and immediate sub-bucket segment names, matching
// bbolt semantics.
type PostgresDB struct {
	db     *sql.DB
	driver string
}

const pgSettingsTable = "gostorm_settings"

// ph renders a positional placeholder for the active driver: $N for pgx (PostgreSQL),
// ? for the in-process test driver. Keeps the KV SQL portable so the identical code
// path is exercised offline in tests and against real Postgres in production/CI.
func (v *PostgresDB) ph(n int) string {
	if v.driver == "pgx" {
		return fmt.Sprintf("$%d", n)
	}
	return "?"
}

func pgBlobType(driver string) string {
	if driver == "pgx" {
		return "BYTEA"
	}
	return "BLOB"
}

// NewPostgresDB opens the PostgreSQL settings backend at dsn (a standard libpq/pgx
// connection string / URL naming the gostream logical database). It verifies the
// connection and ensures the settings table exists.
func NewPostgresDB(dsn string) (GoStormDB, error) {
	return newPostgresDBWithDriver("pgx", dsn)
}

// newPostgresDBWithDriver is the driver-parameterized constructor. Production uses the
// "pgx" driver; tests use an in-process "sqlite" driver over the SAME code path.
func newPostgresDBWithDriver(driver, dsn string) (GoStormDB, error) {
	db, err := sql.Open(driver, dsn)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", driver, err)
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping %s: %w", driver, err)
	}
	v := &PostgresDB{db: db, driver: driver}
	if err := v.ensureSchema(); err != nil {
		db.Close()
		return nil, err
	}
	return v, nil
}

// NewPostgresDBFromEnv returns a PostgresDB when GOSTORM_PG_DSN is set (opt-in
// externalization of config.db onto the shared Postgres server). When the variable is
// unset it returns (nil, nil) so callers keep the unchanged bbolt/json behavior — this
// makes enabling Postgres a purely additive, backward-compatible switch.
func NewPostgresDBFromEnv() (GoStormDB, error) {
	dsn := strings.TrimSpace(os.Getenv("GOSTORM_PG_DSN"))
	if dsn == "" {
		return nil, nil
	}
	return NewPostgresDB(dsn)
}

func (v *PostgresDB) ensureSchema() error {
	q := fmt.Sprintf(
		"CREATE TABLE IF NOT EXISTS %s (xpath TEXT NOT NULL, name TEXT NOT NULL, value %s, PRIMARY KEY (xpath, name))",
		pgSettingsTable, pgBlobType(v.driver))
	if _, err := v.db.Exec(q); err != nil {
		return fmt.Errorf("create %s: %w", pgSettingsTable, err)
	}
	return nil
}

func (v *PostgresDB) CloseDB() {
	if v.db != nil {
		v.db.Close()
		v.db = nil
	}
}

func (v *PostgresDB) Get(xpath, name string) []byte {
	if xpath == "" {
		return nil
	}
	q := fmt.Sprintf("SELECT value FROM %s WHERE xpath=%s AND name=%s", pgSettingsTable, v.ph(1), v.ph(2))
	var val []byte
	err := v.db.QueryRow(q, xpath, name).Scan(&val)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		log.TLogln("Error pg get", xpath+"/"+name, ", error:", err)
		return nil
	}
	return val
}

func (v *PostgresDB) Set(xpath, name string, value []byte) {
	if xpath == "" {
		return
	}
	q := fmt.Sprintf(
		"INSERT INTO %s (xpath, name, value) VALUES (%s, %s, %s) ON CONFLICT (xpath, name) DO UPDATE SET value=EXCLUDED.value",
		pgSettingsTable, v.ph(1), v.ph(2), v.ph(3))
	if _, err := v.db.Exec(q, xpath, name, value); err != nil {
		log.TLogln("Error pg put", xpath+"/"+name, ", error:", err)
	}
}

func (v *PostgresDB) List(xpath string) []string {
	if xpath == "" {
		return nil
	}
	seen := map[string]bool{}
	var ret []string
	add := func(s string) {
		if s != "" && !seen[s] {
			seen[s] = true
			ret = append(ret, s)
		}
	}

	// Direct value keys stored at this bucket path.
	q1 := fmt.Sprintf("SELECT name FROM %s WHERE xpath=%s", pgSettingsTable, v.ph(1))
	if rows, err := v.db.Query(q1, xpath); err == nil {
		for rows.Next() {
			var n string
			if err := rows.Scan(&n); err == nil {
				add(n)
			}
		}
		rows.Close()
	} else {
		log.TLogln("Error pg list", xpath, ", error:", err)
	}

	// Immediate sub-bucket segment names (children of this bucket).
	prefix := xpath + "/"
	q2 := fmt.Sprintf("SELECT DISTINCT xpath FROM %s WHERE xpath LIKE %s", pgSettingsTable, v.ph(1))
	if rows, err := v.db.Query(q2, prefix+"%"); err == nil {
		for rows.Next() {
			var xp string
			if err := rows.Scan(&xp); err != nil {
				continue
			}
			rest := strings.TrimPrefix(xp, prefix)
			if i := strings.IndexByte(rest, '/'); i >= 0 {
				rest = rest[:i]
			}
			add(rest)
		}
		rows.Close()
	} else {
		log.TLogln("Error pg list children", xpath, ", error:", err)
	}

	return ret
}

func (v *PostgresDB) Rem(xpath, name string) {
	if xpath == "" {
		return
	}
	q := fmt.Sprintf("DELETE FROM %s WHERE xpath=%s AND name=%s", pgSettingsTable, v.ph(1), v.ph(2))
	if _, err := v.db.Exec(q, xpath, name); err != nil {
		log.TLogln("Error pg rem", xpath+"/"+name, ", error:", err)
	}
}

func (v *PostgresDB) Clear(xpath string) {
	if xpath == "" {
		return
	}
	// Mirror bbolt Clear: remove the direct value keys stored at this bucket path
	// (nested sub-buckets remain, exactly as bbolt's bucket.ForEach/Delete does).
	q := fmt.Sprintf("DELETE FROM %s WHERE xpath=%s", pgSettingsTable, v.ph(1))
	if _, err := v.db.Exec(q, xpath); err != nil {
		log.TLogln("Error pg clear", xpath, ", error:", err)
	}
}
