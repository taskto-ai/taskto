package audit

import "slices"

func Actions() []string {
	var all []string
	for a := range schemas {
		all = append(all, a)
	}
	slices.Sort(all)
	return all
}

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

func SecretName(name string) bool { return secretName.MatchString(name) }

func TargetTypes() []string {
	var all []string
	for t := range targetTables {
		all = append(all, t)
	}
	slices.Sort(all)
	return all
}

func TargetTable(typ string) string { return targetTables[typ] }

func ListSQL(workspace int64, q Query) (string, []any, error) {
	stmt, args, _, err := listSQL(workspace, q)
	return stmt, args, err
}
