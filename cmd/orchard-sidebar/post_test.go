package main

// Regression tests for post(): a GraphQL 200 carrying BOTH a populated data
// payload and a non-empty errors array is a partial success, not a total
// failure. The daemon emits exactly that shape when GitHub rate-limits the
// pr/issue leaves while every other field resolves normally; discarding data
// in that case blanks every github field in the sidebar at once.
//
// The guard being fixed is still correct for the zero-data case, so the cases
// below pin both sides of the boundary rather than only the side that
// regressed -- a fix that simply drops the errors check would pass case 1 and
// fail case 2.

import (
	"net/http"
	"testing"
	"time"
)

// The wire shape observed on the box: every leaf resolved except the github
// ones, which report the rate limit through errors[].
const rateLimitedPartial = `{
  "data": {"workView": {"repos": [
    {"slug": "drewdrewthis/orchardist", "worktrees": [{"path": "/home/ubuntu/workspace/git-orchard-rs"}]}
  ]}},
  "errors": [{"message": "github API rate limited; resets at 1786742279"}]
}`

const errorsOnlyNoData = `{"data": null, "errors": [{"message": "daemon: work view unavailable"}]}`

const cleanData = `{"data": {"workView": {"repos": [
  {"slug": "drewdrewthis/orchardist", "worktrees": [{"path": "/home/ubuntu/workspace/git-orchard-rs"}]}
]}}}`

func TestPostPartialSuccessKeepsData(t *testing.T) {
	tests := []struct {
		name      string
		body      string
		wantErr   bool
		wantRepos int
	}{
		{
			name:      "errors alongside populated data still yields the data",
			body:      rateLimitedPartial,
			wantErr:   false,
			wantRepos: 1,
		},
		{
			name:      "errors with null data remains a failure",
			body:      errorsOnlyNoData,
			wantErr:   true,
			wantRepos: 0,
		},
		{
			name:      "clean response decodes",
			body:      cleanData,
			wantErr:   false,
			wantRepos: 1,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fakeGraphQL(t, http.StatusOK, tc.body)

			var out slowResp
			err := post(slowQuery, 2*time.Second, &out)

			if tc.wantErr && err == nil {
				t.Fatalf("post: want error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("post: want no error, got %v", err)
			}
			if got := len(out.Data.WorkView.Repos); got != tc.wantRepos {
				t.Errorf("repos decoded = %d, want %d", got, tc.wantRepos)
			}
		})
	}
}
