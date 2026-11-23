package categories

import "strings"

// seedAliases lists, per default category key, the names that mean the same thing in the
// supported languages (en and id). A new or renamed category whose name is one of these while the
// space already has a category of the same meaning and type gets CATEGORY_EXISTS_SIMILAR.
// All entries are lower case.
var seedAliases = map[string][]string{
	"TRANSPORT":     {"transport", "transportasi", "transpor"},
	"SHOPPING":      {"shopping", "belanja"},
	"SUBSCRIPTIONS": {"subscriptions", "subscription", "langganan"},
	"HEALTH":        {"health", "kesehatan"},
	"GIFTS":         {"gifts", "gift", "hadiah", "kado"},
	"SNACKS":        {"snacks", "snack", "jajan", "camilan", "cemilan"},
	"FAMILY":        {"family", "keluarga"},
	"SALARY":        {"salary", "gaji", "pendapatan"},
	"FREELANCE":     {"freelance", "freelancer", "lepas"},
	"INVESTMENT":    {"investment", "investasi"},
	"DIVIDEND":      {"dividend", "dividen"},
}

// aliasGroup returns the default-category key a name is an alias of, or "".
func aliasGroup(name string) string {
	n := strings.ToLower(strings.TrimSpace(name))
	for key, names := range seedAliases {
		for _, a := range names {
			if a == n {
				return key
			}
		}
	}
	return ""
}
