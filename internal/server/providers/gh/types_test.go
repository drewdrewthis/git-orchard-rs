package gh_test

import (
	"testing"

	"github.com/drewdrewthis/orchardist/internal/server/providers/gh"
)

// TestPullRequest_UnresolvedThreadCount pins UnresolvedThreadCount to
// GitHub's actual "Require conversation resolution before merging" gate,
// which blocks on isResolved == false alone — isOutdated has no bearing on
// it. An outdated-but-unresolved thread must still count (#607 / PR #764
// review finding); only a resolved thread is excluded, regardless of its
// outdated state.
func TestPullRequest_UnresolvedThreadCount(t *testing.T) {
	tests := []struct {
		name    string
		threads []gh.ReviewThread
		want    int
	}{
		{
			name:    "outdated but unresolved still blocks merge",
			threads: []gh.ReviewThread{{IsResolved: false, IsOutdated: true}},
			want:    1,
		},
		{
			name:    "resolved and outdated does not block merge",
			threads: []gh.ReviewThread{{IsResolved: true, IsOutdated: true}},
			want:    0,
		},
		{
			name:    "resolved and not outdated does not block merge",
			threads: []gh.ReviewThread{{IsResolved: true, IsOutdated: false}},
			want:    0,
		},
		{
			name:    "unresolved and not outdated blocks merge",
			threads: []gh.ReviewThread{{IsResolved: false, IsOutdated: false}},
			want:    1,
		},
		{
			name: "mixed threads count every unresolved one regardless of outdated",
			threads: []gh.ReviewThread{
				{IsResolved: false, IsOutdated: false},
				{IsResolved: true, IsOutdated: false},
				{IsResolved: false, IsOutdated: true},
				{IsResolved: true, IsOutdated: true},
			},
			want: 2,
		},
		{
			name:    "no threads",
			threads: nil,
			want:    0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pr := gh.PullRequest{ReviewThreads: tt.threads}
			if got := pr.UnresolvedThreadCount(); got != tt.want {
				t.Errorf("UnresolvedThreadCount() = %d, want %d", got, tt.want)
			}
		})
	}
}
