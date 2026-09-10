// Copyright (c) 2026 Kristoffer Dalby
// SPDX-License-Identifier: BSD-3-Clause

package cli

import (
	"encoding/json"
	"errors"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/peterbourgon/ff/v4"
	"github.com/stretchr/testify/require"

	"github.com/kradalby/tsnixcache/watch"
)

// TestUnknownSubcommandNamesIt: a typo used to print generic usage plus "flag:
// help requested" and never say which word was wrong.
func TestUnknownSubcommandNamesIt(t *testing.T) {
	err := newRootCmd().ParseAndRun(t.Context(), []string{"psuh"})
	if !errors.Is(err, errUnknownSubcommand) {
		t.Fatalf("tsnixcache psuh = %v, want errUnknownSubcommand", err)
	}

	if !strings.Contains(err.Error(), "psuh") {
		t.Errorf("error %q does not name the offending word", err)
	}
}

func TestVersionSubcommand(t *testing.T) {
	err := newRootCmd().ParseAndRun(t.Context(), []string{"version"})
	if err != nil {
		t.Fatalf("tsnixcache version: %v", err)
	}

	err = newRootCmd().ParseAndRun(t.Context(), []string{"--version"})
	if err != nil {
		t.Fatalf("tsnixcache --version: %v", err)
	}
}

const watchCommand = "watch"

func TestModuleDurationContract(t *testing.T) {
	path := os.Getenv("TSNIXCACHE_DURATION_CASES")
	if path == "" {
		t.Skip("module evaluation supplied by checks.duration-types")
	}

	data, err := os.ReadFile(path) // #nosec G304 G703 -- Nix supplies the contract fixture.
	require.NoError(t, err)

	var cases []struct {
		Command  string `json:"command"`
		Flag     string `json:"flag"`
		Value    string `json:"value"`
		Option   string `json:"option"`
		Accepted bool   `json:"accepted"`
		Nanos    *int64 `json:"nanos"`
	}
	require.NoError(t, json.Unmarshal(data, &cases))
	require.NotEmpty(t, cases)

	whole := regexp.MustCompile(`^(0|[0-9]+d|([0-9]+(ns|us|ms|s|m|h))+)$`)

	for _, test := range cases {
		t.Run(test.Command+"/"+test.Flag+"/"+test.Value, func(t *testing.T) {
			var cmd *ff.Command

			switch test.Command {
			case "serve":
				cmd = newServeCmd()
			case watchCommand:
				cmd = newWatchCmd()
			case "push":
				cmd = newPushCmd()
			default:
				t.Fatalf("unknown command %q", test.Command)
			}

			value := test.Value
			if test.Flag == "gc-rule" {
				value = "80:" + value
			}

			parseErr := cmd.Parse([]string{"--" + test.Flag, value})

			var duration time.Duration

			if parseErr == nil {
				flag, ok := cmd.Flags.GetFlag(test.Flag)
				require.True(t, ok)

				switch test.Flag {
				case "gc-rule":
					var rule GCRule

					rule, parseErr = parseGCRule(flag.GetValue())
					duration = rule.OlderThan
				case "gc-interval":
					duration, parseErr = parseGCInterval(flag.GetValue())
				default:
					parseErr = (dayDuration{&duration}).Set(flag.GetValue())
					if parseErr == nil && test.Command == watchCommand {
						parseErr = validateWatchOptions(&watch.Watcher{
							Attempts: 1, StallTimeout: time.Second, PollInterval: duration,
							RetryBackoffBase: time.Second, RetryBackoffMax: time.Second,
						}, nil)
					}
				}
			}

			require.Equal(t, parseErr == nil && whole.MatchString(test.Value), test.Accepted, "%s: %v", test.Option, parseErr)

			if test.Accepted {
				require.NotNil(t, test.Nanos)
				require.Equal(t, *test.Nanos, int64(duration), test.Option)
			}
		})
	}
}
