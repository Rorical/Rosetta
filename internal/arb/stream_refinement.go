package arb

func mergeSearchResults(result, extra SearchResult) SearchResult {
	result.Selected += extra.Selected
	result.Published += extra.Published
	result.ExactCanceled += extra.ExactCanceled
	result.ExactReused += extra.ExactReused
	result.ProbeOnly += extra.ProbeOnly
	result.Limits.add(extra.Limits)
	for _, issue := range extra.Issues {
		result.Issues = appendIssue(result.Issues, issue)
	}
	return result
}
