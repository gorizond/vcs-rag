package provider

// MergeRepos deduplicates repositories by ID, keeping first-appearance order.
// Input lists are hundreds of entries; O(n) memory is acceptable.
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
