package cli

import "testing"

// TestBuildIdentityRendersWhatItHas covers the shape `kno --version` prints.
//
// It matters more than its size suggests: SECURITY.md, README.md and
// install.sh all quote this output, and the release Makefile target asserts on
// it to catch an -X path that names the wrong symbol. If the rendering changes,
// three documents and one gate start lying at once.
func TestBuildIdentityRendersWhatItHas(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		id   buildIdentity
		want string
	}{
		{
			name: "a hand-built binary knows only that it is not a release",
			id:   buildIdentity{Version: "dev"},
			want: "dev",
		},
		{
			name: "a released binary carries all three",
			id:   buildIdentity{Version: "v0.0.1", Commit: "abc1234", Date: "2026-08-26T00:00:00Z"},
			want: "v0.0.1 (abc1234, 2026-08-26T00:00:00Z)",
		},
		{
			name: "a go install binary has a version and a revision but no date",
			id:   buildIdentity{Version: "v0.0.1", Commit: "abc1234"},
			want: "v0.0.1 (abc1234)",
		},
		{
			name: "a dirty tree says so, because that build is reproducible by nobody",
			id:   buildIdentity{Version: "dev", Commit: "abc1234-dirty"},
			want: "dev (abc1234-dirty)",
		},
		{
			name: "a date without a revision still beats printing nothing",
			id:   buildIdentity{Version: "v0.0.1", Date: "2026-08-26T00:00:00Z"},
			want: "v0.0.1 (2026-08-26T00:00:00Z)",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.id.String(); got != tt.want {
				t.Errorf("String() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestIdentityNeverReportsTheToolchainPlaceholder pins the one branch that is
// easy to get backwards.
//
// `go build` with no VCS metadata reports Main.Version as "(devel)". Taking it
// would replace "dev" — which reads as "you built this yourself" — with a
// string that looks like a broken release. The fallback exists to IMPROVE on
// "dev", never to degrade it.
func TestIdentityNeverReportsTheToolchainPlaceholder(t *testing.T) {
	t.Parallel()

	got := identity()
	if got.Version == "(devel)" || got.Version == "" {
		t.Errorf("identity().Version = %q; a placeholder is worse than \"dev\"", got.Version)
	}
}
