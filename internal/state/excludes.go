package state

import "fmt"

// Exclude rule types (structured filter model, §10.1).
const (
	RuleExcludePathPrefix = "exclude_path_prefix"
	RuleExcludeDirName    = "exclude_dir_name"
	RuleExcludeFileName   = "exclude_filename"
	RuleExcludeExtension  = "exclude_extension"
)

// ValidRuleType reports whether ruleType is a supported structured rule.
func ValidRuleType(ruleType string) bool {
	switch ruleType {
	case RuleExcludePathPrefix, RuleExcludeDirName, RuleExcludeFileName, RuleExcludeExtension:
		return true
	}
	return false
}

// GetExcludes returns exclude rules for a profile as "rule_type:rule_value"
// strings, preserving the structured filter semantics (§10.1).
func (d *DB) GetExcludes(profileID string) ([]string, error) {
	rows, err := d.Query(`SELECT rule_type, rule_value FROM profile_excludes
		WHERE profile_id = ? ORDER BY rule_type, rule_value`, profileID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var t, v string
		if err := rows.Scan(&t, &v); err != nil {
			return nil, err
		}
		out = append(out, t+":"+v)
	}
	return out, rows.Err()
}

// AddExclude inserts a single rule.
func (d *DB) AddExclude(profileID, ruleType, ruleValue string) error {
	if !ValidRuleType(ruleType) {
		return fmt.Errorf("invalid exclude rule type %q", ruleType)
	}
	_, err := d.Exec(`INSERT OR IGNORE INTO profile_excludes (profile_id, rule_type, rule_value)
		VALUES (?, ?, ?)`, profileID, ruleType, ruleValue)
	return err
}

// RemoveExclude deletes a single rule.
func (d *DB) RemoveExclude(profileID, ruleType, ruleValue string) error {
	res, err := d.Exec(`DELETE FROM profile_excludes WHERE profile_id = ? AND rule_type = ? AND rule_value = ?`,
		profileID, ruleType, ruleValue)
	if err != nil {
		return err
	}
	return checkRows(res)
}
