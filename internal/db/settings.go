package db

// GetSetting returns the value for a settings key, or "" if unset.
func (db *DB) GetSetting(key string) string {
	var v string
	db.QueryRow("SELECT value FROM settings WHERE key=?", key).Scan(&v)
	return v
}

// SetSetting upserts a settings key/value pair.
func (db *DB) SetSetting(key, value string) error {
	_, err := db.Exec(`INSERT INTO settings (key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value)
	return err
}
