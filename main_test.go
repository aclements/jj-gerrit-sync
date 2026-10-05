// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import "testing"

func TestResolveTarget(t *testing.T) {
	tests := []struct {
		name       string
		onto       string
		remote     string
		wantBranch string
		wantRemote string
		wantTarget string
		wantErr    bool
	}{
		{
			name:       "explicit branch and remote",
			onto:       "feature@upstream",
			remote:     "",
			wantBranch: "feature",
			wantRemote: "upstream",
			wantTarget: "feature@upstream",
		},
		{
			name:       "explicit branch and matching remote flag",
			onto:       "feature@upstream",
			remote:     "upstream",
			wantBranch: "feature",
			wantRemote: "upstream",
			wantTarget: "feature@upstream",
		},
		{
			name:    "mismatched remote flag",
			onto:    "feature@upstream",
			remote:  "origin",
			wantErr: true,
		},
		{
			name:    "invalid multiple at-signs",
			onto:    "a@b@c",
			wantErr: true,
		},
		{
			name:    "empty branch before at-sign",
			onto:    "@origin",
			wantErr: true,
		},
		{
			name:    "empty remote after at-sign",
			onto:    "master@",
			wantErr: true,
		},
		{
			name:       "onto with explicit remote flag",
			onto:       "master",
			remote:     "upstream",
			wantBranch: "master",
			wantRemote: "upstream",
			wantTarget: "master@upstream",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b, r, target, err := resolveTarget(tc.onto, tc.remote)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if b != tc.wantBranch || r != tc.wantRemote || target != tc.wantTarget {
				t.Errorf("got (%q, %q, %q), want (%q, %q, %q)", b, r, target, tc.wantBranch, tc.wantRemote, tc.wantTarget)
			}
		})
	}
}
