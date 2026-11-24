package audit

import "slices"

// Actions are the actions that have a schema.
func Actions() []string {
	var all []string
	for a := range schemas {
		all = append(all, a)
	}
	slices.Sort(all)
	return all
}

// Schemas gives, for each action, each of its details and whether it is a boolean.
func Schemas() map[string]map[string]bool {
	out := map[string]map[string]bool{}
	for a, s := range schemas {
		out[a] = map[string]bool{}
		for name, k := range s {
			out[a][name] = k.boolean
		}
	}
	return out
}

// SecretName says whether a detail's name is the name of a secret.
func SecretName(name string) bool { return secretName.MatchString(name) }

// TargetTypes are the types of target whose creation time is recorded.
func TargetTypes() []string {
	var all []string
	for t := range targetTables {
		all = append(all, t)
	}
	slices.Sort(all)
	return all
}

// TargetTable is the table of a target type.
func TargetTable(typ string) string { return targetTables[typ] }
