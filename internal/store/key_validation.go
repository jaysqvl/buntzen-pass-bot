package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"

	secretcrypto "github.com/jaysqvl/lake-pass-bot/internal/crypto"
)

var ErrKeyMismatch = errors.New("database contains encrypted data that cannot be authenticated; restore its matching key and intact data")

func verifyExistingDatabaseKey(ctx context.Context, path string, box *secretcrypto.Encryptor) error {
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("inspect existing database: %w", err)
	}
	// mode=ro is deliberately WAL-aware. immutable=1 could miss a credential
	// committed to WAL and would disable SQLite's normal locking assumptions.
	databaseURL := (&url.URL{Scheme: "file", Path: path}).String()
	database, err := sql.Open("sqlite", databaseURL+"?mode=ro&_pragma=busy_timeout(5000)&_pragma=query_only(1)")
	if err != nil {
		return fmt.Errorf("open database for key validation: %w", err)
	}
	defer database.Close()
	database.SetMaxOpenConns(1)
	return verifyEncryptedRecords(ctx, database, box)
}

func verifyEncryptedRecords(ctx context.Context, database *sql.DB, box *secretcrypto.Encryptor) error {
	for _, field := range []struct {
		table, column string
		limit         int
		allowEmpty    bool
	}{
		{"otp_sources", "config_ciphertext", 8192, false},
		{"profiles", "yodel_phone_ciphertext", 4096, true},
		{"profiles", "yodel_email_ciphertext", 4096, true},
		{"profiles", "yodel_password_ciphertext", 4096, true},
	} {
		var exists bool
		if err := database.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM pragma_table_info(?) WHERE name = ?)", field.table, field.column).Scan(&exists); err != nil {
			return fmt.Errorf("inspect encrypted data schema: %w", err)
		}
		if !exists {
			continue
		}
		// Identifiers are fixed source constants. CASE bounds materialized input
		// even if a damaged database no longer has the schema's length constraint.
		query := fmt.Sprintf(`SELECT CASE WHEN length(CAST(%s AS BLOB)) <= ? THEN %s ELSE NULL END FROM %s`, field.column, field.column, field.table)
		rows, err := database.QueryContext(ctx, query, field.limit)
		if err != nil {
			return fmt.Errorf("read encrypted data for key validation: %w", err)
		}
		for rows.Next() {
			var ciphertext sql.NullString
			if err := rows.Scan(&ciphertext); err != nil {
				rows.Close()
				return fmt.Errorf("read encrypted value: %w", err)
			}
			if !ciphertext.Valid {
				rows.Close()
				return ErrKeyMismatch
			}
			if ciphertext.String == "" && field.allowEmpty {
				continue
			}
			plaintext, err := box.Decrypt(ciphertext.String)
			clear(plaintext)
			if err != nil {
				rows.Close()
				return ErrKeyMismatch
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return fmt.Errorf("validate encrypted data: %w", err)
		}
	}
	return nil
}
