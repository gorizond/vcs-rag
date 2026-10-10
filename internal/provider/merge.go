package provider

// MergeRepos дедуплицирует репозитории по ID, сохраняя порядок первого
// появления. Входные списки — сотни элементов, O(n) по памяти приемлемо.
func MergeRepos(repos []Repo) []Repo {
	seen := make(map[string]bool, len(repos))
	out := make([]Repo, 0, len(repos))
	for _, r := range repos {
		if seen[r.ID] {
			continue
		}
		seen[r.ID] = true
		out = append(out, r)
	}
	return out
}
