package config

// DefaultSessionRetentionDays is how long a session is kept after its last
// activity when neither the project nor the user says otherwise.
const DefaultSessionRetentionDays = 30

// SessionRetention returns how many days sessions are kept after their last
// activity, and where that number came from ("project", "user" or "default").
// 0 means keep forever; a negative setting is read as 0.
//
// Resolution: the project's config.json, then the user-level prefs.json, then
// DefaultSessionRetentionDays.
func SessionRetention(cfg *Config) (days int, source string) {
	switch {
	case cfg != nil && cfg.SessionRetentionDays != nil:
		days, source = *cfg.SessionRetentionDays, "project"
	default:
		if p := loadUserPrefs(); p.SessionRetentionDays != nil {
			days, source = *p.SessionRetentionDays, "user"
		} else {
			days, source = DefaultSessionRetentionDays, "default"
		}
	}
	return max(days, 0), source
}

// SaveUserSessionRetention records the user-level retention (nil clears it,
// back to the default). Other user-level settings are preserved.
func SaveUserSessionRetention(days *int) error {
	p := loadUserPrefs()
	p.SessionRetentionDays = days
	return writeUserPrefs(p)
}
